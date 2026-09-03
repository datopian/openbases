#!/usr/bin/env bash
# The remote MCP server, over the wire, against staging (wg-p4h.11).
#
# Three things this proves and one it deliberately does not.
#
# It proves the transport works end to end: initialize negotiates, tools/list
# returns the curated set, and workgraph_inbox answers with the same items the
# Needs-you page shows. It proves an unauthenticated request is refused. It
# proves a request claiming somebody else's Origin is refused.
#
# It does NOT prove that a real client can log in, because that needs a browser
# and a person. The verification matrix in the PR covers that, by hand, and this
# script covers everything after the token exists.
#
# THE TOKEN IS THE TEST RUNNER'S, and it is read from the environment rather
# than minted here. WG_MCP_ACCESS_TOKEN holds an Access token obtained by a
# client login; without it the script skips rather than failing, so CI can run
# it and report honestly that it had no credential.
set -uo pipefail

BASE="${WG_MCP_BASE:-https://work-staging.openbases.com}"
MCP="$BASE/mcp"
TOKEN="${WG_MCP_ACCESS_TOKEN:-}"

pass=0
fail=0
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail + 1)); }
note() { printf '  \033[33mSKIP\033[0m  %s\n' "$1"; }

echo "remote MCP acceptance against $MCP"
echo "----------------------------------------"

# --- unauthenticated ------------------------------------------------------
#
# Access answers this, not the origin. The distinction matters and is the whole
# point of the change: if control-api's own 401 comes back, Access is not
# intercepting and Managed OAuth is not in effect.
unauth_headers=$(curl -sS -i -m 30 -X POST "$MCP" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' 2>/dev/null | tr -d '\r')

status=$(printf '%s' "$unauth_headers" | head -1 | awk '{print $2}')
if [ "$status" = "401" ]; then
  ok "an unauthenticated initialize is refused with 401"
else
  bad "an unauthenticated initialize returned $status, want 401"
fi

wa=$(printf '%s' "$unauth_headers" | grep -i '^www-authenticate:' | head -1)
if [ -n "$wa" ]; then
  ok "the refusal carries WWW-Authenticate"
else
  # Without Managed OAuth, Access answers a non-browser client the way it
  # answers a browser: a 302 to the login page and nothing a client can act on.
  bad "no WWW-Authenticate header; Managed OAuth is probably not enabled on the Access application"
fi

# The discovery URL is READ from the header rather than guessed.
#
# The first version of this script asserted
# /.well-known/oauth-authorization-server on our own hostname, which returned
# 302 because that document is the authorization SERVER's and lives on the team
# domain. What Access actually advertises is RFC 9728 protected-resource
# metadata, path-scoped to the route:
#
#   www-authenticate: Cloudflare-Access
#     resource_metadata="https://<host>/.well-known/cloudflare-access-protected-resource/mcp"
#
# Following the advertised URL is also what a real client does, so this tests
# the path a client takes instead of one we assumed it takes.
meta_url=$(printf '%s' "$wa" | sed -n 's/.*resource_metadata="\([^"]*\)".*/\1/p')
if [ -n "$meta_url" ]; then
  ok "WWW-Authenticate advertises resource metadata"
else
  bad "WWW-Authenticate names no resource_metadata: $wa"
fi

if [ -n "$meta_url" ]; then
  meta=$(curl -sS -m 30 "$meta_url" 2>/dev/null)
  if printf '%s' "$meta" | grep -q '"authorization_servers"'; then
    ok "the resource metadata names an authorization server"
  else
    bad "the resource metadata has no authorization_servers: $meta"
  fi
  # The signature of Managed OAuth being ON: an oauth authentication method
  # appears beside cloudflared. Before it is enabled the document lists only
  # cloudflared, which no hosted client can use.
  if printf '%s' "$meta" | grep -qi '"name": *"oauth\|oauth2\|authorization_code'; then
    ok "an OAuth authentication method is advertised"
  else
    bad "the metadata advertises no OAuth method; a hosted client has no way in$(printf '\n        ')$meta"
  fi
fi

# --- everything below needs a token ---------------------------------------
if [ -z "$TOKEN" ]; then
  echo
  note "WG_MCP_ACCESS_TOKEN is not set, so the authenticated cases did not run"
  note "obtain one by connecting a client, then export it for this runner only"
  echo
  echo "----------------------------------------"
  printf 'passed %d, failed %d\n' "$pass" "$fail"
  [ "$fail" -eq 0 ] || exit 1
  exit 0
fi

# One JSON-RPC call. Streamable HTTP wants both content types in Accept: the
# server may answer a POST with JSON or open an SSE stream, and a client that
# advertises only one gets a 406 from a correct server.
rpc() {
  curl -sS -m 60 -X POST "$MCP" \
    -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    ${SESSION:+-H "Mcp-Session-Id: $SESSION"} \
    -d "$1" 2>/dev/null
}

# initialize, keeping the session id the server assigns.
init_out=$(curl -sS -i -m 60 -X POST "$MCP" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"wg-acceptance","version":"1"}}}' \
  2>/dev/null | tr -d '\r')

if printf '%s' "$init_out" | grep -q '"protocolVersion"'; then
  ok "initialize negotiated a protocol version"
else
  bad "initialize did not negotiate: $(printf '%s' "$init_out" | tail -3)"
fi

SESSION=$(printf '%s' "$init_out" | grep -i '^mcp-session-id:' | awk '{print $2}')
if [ -n "$SESSION" ]; then
  ok "the server assigned a session id"
else
  # Not fatal: a stateless server is a legitimate configuration and the spec
  # allows it. Recorded rather than failed.
  note "no Mcp-Session-Id was assigned; the server is running stateless"
fi

# The initialized notification, which the spec requires before other calls.
rpc '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null

tools=$(rpc '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}')
for t in workgraph_inbox workgraph_ask workgraph_work_list workgraph_project_list \
         workgraph_file_work workgraph_dispatch; do
  if printf '%s' "$tools" | grep -q "\"$t\""; then
    ok "tools/list advertises $t"
  else
    bad "tools/list is missing $t"
  fi
done

# The two that spend money must not be advertised as read-only, or a client
# will run them without asking anybody.
if printf '%s' "$tools" | python3 -c '
import json,sys
raw=sys.stdin.read()
# The answer may arrive as SSE frames; take the last JSON object on a data line.
payload=None
for line in raw.splitlines():
    line=line.strip()
    if line.startswith("data:"):
        line=line[5:].strip()
    if line.startswith("{"):
        try: payload=json.loads(line)
        except Exception: pass
if payload is None: sys.exit(2)
tools={t["name"]: t for t in payload.get("result",{}).get("tools",[])}
bad=[]
for name in ("workgraph_file_work","workgraph_dispatch"):
    a=tools.get(name,{}).get("annotations",{}) or {}
    if a.get("readOnlyHint"): bad.append(name)
sys.exit(1 if bad else 0)
'; then
  ok "the spending tools are not advertised read-only"
else
  bad "a tool that spends money is advertised read-only, so clients will not prompt"
fi

inbox=$(rpc '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"workgraph_inbox","arguments":{}}}')
if printf '%s' "$inbox" | grep -q '"content"'; then
  ok "workgraph_inbox returned content"
else
  bad "workgraph_inbox returned no content: $(printf '%s' "$inbox" | tail -3)"
fi
if printf '%s' "$inbox" | grep -q '"isError":true'; then
  bad "workgraph_inbox came back as an error: $(printf '%s' "$inbox" | tail -3)"
else
  ok "workgraph_inbox was not an error"
fi

# --- origin ----------------------------------------------------------------
#
# A page on somebody else's site must not be able to script a call and read the
# answer. This is the case that fails SILENTLY when the allow-list is wrong, so
# it is asserted with a credential attached: a refusal here is about the origin
# and nothing else.
evil=$(curl -sS -o /dev/null -w '%{http_code}' -m 30 -X POST "$MCP" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'Origin: https://evil.example' \
  -d '{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}' 2>/dev/null)
if [ "$evil" = "403" ]; then
  ok "a request claiming another origin is refused with 403"
else
  bad "a request from https://evil.example returned $evil, want 403"
fi

own=$(curl -sS -o /dev/null -w '%{http_code}' -m 30 -X POST "$MCP" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H "Origin: $BASE" \
  -d '{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{}}' 2>/dev/null)
if [ "$own" = "200" ]; then
  ok "a request from our own origin is served"
else
  bad "a request from $BASE returned $own, want 200 — the web client would fail silently like this"
fi

echo
echo "----------------------------------------"
printf 'passed %d, failed %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
