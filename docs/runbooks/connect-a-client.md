# Runbook: connect a client to Workgraph

**For:** anyone at Datopian who wants Workgraph's tools inside Claude or Codex
**Applies to:** staging (`https://work-staging.openbases.com`) — see ADR-0028

Workgraph exposes a remote MCP server at `/mcp`. Adding it as a connector gives
your client six tools: your inbox, the ask endpoint, the work list, the project
list, and the two that file and run work.

**You will be asked to log in with Google.** That is expected and is the whole
design: the connector has no password and no pasted token. Your client opens
your browser, you complete the normal Workgraph login — Google Workspace, MFA,
the same allow-list as the web interface — and the client receives a token
Cloudflare Access issues and enforces. Workgraph itself never sees a password
and implements no OAuth (ADR-0028).

What you see through a connector is exactly what you see in the browser: the
same user record, the same project memberships, the same row-level security. A
connector is not an elevated path.

## Connect

### Claude desktop, Cowork, claude.ai web, and the phone apps

One connector serves all of them, because Claude connects to a custom connector
from Anthropic's cloud rather than from your device.

1. Settings → Connectors → **Add custom connector**
2. URL: `https://work-staging.openbases.com/mcp`
3. Save, then **Connect**. Your browser opens the Google login.
4. The tool list appears. Try "what needs me" — it should match the
   **Needs you** page.

### Claude Code

```bash
claude mcp add --transport http workgraph https://work-staging.openbases.com/mcp
```

Then run any Workgraph prompt; Claude Code opens the browser on first use.
It binds a random local port for the OAuth callback. If you need a fixed one,
`--callback-port` sets it.

### Codex CLI

```bash
codex mcp add workgraph --url https://work-staging.openbases.com/mcp
```

Verify the flag spelling against your Codex version — CLI flags here have
changed between releases and a wrong one fails quietly. If Codex's remote MCP
does not complete the OAuth discovery flow, fall back to stdio, which needs no
connector and no OAuth:

```bash
wg login                 # once
codex mcp add workgraph --command wg --args mcp
```

`wg mcp` serves the same six tools from the same code, so nothing is lost except
the ability to use it from a machine without `wg`.

## Add the connector for everyone (Claude Team/Enterprise admins)

Settings → Connectors → the organisation tab → **Add custom connector**, same
URL. Each person still logs in individually the first time: an
organisation-level connector shares the URL, not the credential.

Only people on the Access allow-list can complete the login. Adding the
connector org-wide does not grant anybody access to Workgraph.

## Revoke

Access is the single place, and there are two levels.

**End one person's sessions**, without removing them:
Zero Trust → Access controls → Applications → the Workgraph application → their
session. Their next tool call fails with an authentication error rather than a
stale success.

**Remove them entirely**: take the address out of `access_allowed_emails` and
apply.

```bash
scripts/with_secrets.sh staging scripts/tofu.sh staging apply
```

Their next tool call fails. There is no Workgraph-side token to hunt for,
because Workgraph never issued one — that is the property the whole design was
chosen for.

## When it does not work

### "Authorization with Workgraph failed" after the login succeeded

You completed the Google login and the client then refused. The login working
means Access is configured; what failed is how the client identified ITSELF.

**Set the connector's OAuth client mode to DCR, not the recommended one.**

Claude offers three:

| Option | Works here |
|---|---|
| *Use Anthropic's hosted client metadata (CIMD)* — marked **Recommended** | **No.** Access does not support it. |
| *No client ID — register one automatically (DCR)* — marked *Detected* | **Yes.** Use this. |
| *Use your own OAuth client* | Works, if you registered one yourself. |

The recommended option is the one that cannot work, which is why this is worth
writing down. CIMD has the client present its `client_id` as a URL that the
authorization server fetches to learn about it. Access does not advertise
support, and its metadata is the whole list of what it does:

```bash
curl -sS https://datopian.cloudflareaccess.com/.well-known/oauth-authorization-server | python3 -m json.tool
```

```
authorization_endpoint, token_endpoint, registration_endpoint,
revocation_endpoint, grant_types_supported [authorization_code, refresh_token],
code_challenge_methods_supported [S256], response_types_supported [code],
token_endpoint_auth_methods_supported [client_secret_basic, client_secret_post, none]
```

No `client_id_metadata_document_supported`, and no CIMD field of any kind. The
`registration_endpoint` is there, so **DCR is the path Access offers** — which
is what "Detected" beside that option is telling you.

The other two settings are already right and need no change: **Authentication →
Always required**, and **Transport → Streamable HTTP**.

If DCR still fails, the reference the client shows you (`ofid_…`) is
Anthropic's, not ours, and means nothing on this side. Read our half from Zero
Trust → Logs → Access, filtered to the Workgraph application: the failed event
names the `redirect_uri` and the client that asked. The deploy token cannot read
those logs, so this is a dashboard step.

### The client says it cannot authenticate, or shows a raw 302

Managed OAuth is probably not enabled on the Access application. Check what an
unauthenticated request is answered with:

```bash
curl -sS -i -X POST https://work-staging.openbases.com/mcp \
  -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"initialize"}' \
  | grep -iE '^HTTP|^www-authenticate'
```

You want a `401` and a `WWW-Authenticate` header. A `302` means Access is
redirecting to a login page instead of answering a non-browser client, which is
what it does with Managed OAuth off. Confirm `oauth_configuration.enabled` is
`true` in `infra/tofu/modules/environment/main.tf` and applied.

If you get **control-api's own 401** — a JSON body with an `error` field —
Access is not intercepting at all, and the request reached the origin. That is a
different problem: do not work around it, because it means the route is not
protected the way this runbook assumes.

### The hosted clients fail but Claude Code works

The dynamic-registration redirect allow-list. localhost and loopback are
allowed, which covers the command-line clients — Claude Code declares
`http://localhost/callback` and `http://127.0.0.1/callback` with only the port
configurable — and a hosted client is refused at **registration**, before
anybody sees a login page, unless its URI is in
`access_oauth_allowed_redirect_uris`.

Staging allows `https://claude.ai/*` and `https://claude.com/*`, domain-scoped
rather than one exact path — and that is a retreat, worth knowing about.

The exact path was tried first: `https://claude.ai/api/mcp/auth_callback`. It is
a real endpoint (a bare `GET` answers 400, not 404) and it was applied, and a
hosted client still could not connect. That rules out the value and leaves the
registration *request*: a client submitting several `redirect_uris` is refused
outright if any one of them is outside the list, and we cannot see what it
sends without the Access log.

**A localhost client is no use for a hosted client.** Cowork, claude.ai and the
phones redirect to claude.ai; there is no port to pin and no `--callback-port`
to set. Only Claude Code and Codex use loopback. Mixing those two up wastes an
attempt and produces "invalid redirect url" in the browser, which reads like a
server fault and is not one.

**Narrow it back when you can.** Zero Trust → Logs → Access, the failed event
names the URI a real registration asked for. Replacing the wildcards with that
value is strictly better; the wildcards are bought with lack of visibility.

You can check what the allow-list accepts without opening a browser. The
registration endpoint answers honestly, and a refused registration creates
nothing:

```bash
curl -sS -X POST   https://datopian.cloudflareaccess.com/cdn-cgi/access/oauth/registration   -H 'Content-Type: application/json'   -d '{"client_name":"probe","redirect_uris":["https://example.invalid/never-allowed"],
       "grant_types":["authorization_code"],"response_types":["code"],
       "token_endpoint_auth_method":"none"}'
```

A refused URI answers:

```json
{"error":"invalid_client_metadata",
 "error_description":"redirect_uri is not allowed by the account configuration"}
```

An allowed one answers `201` with a `client_id` — which **registers a client**,
so probe with a URI you expect to be refused unless you mean to create one.
Registrations come back without a `registration_access_token`, so there is no
RFC 7592 way to delete them; removal is a dashboard action.

### A tool call is refused

The message says which and what to do about it, in the same terms as the CLI's
exit codes:

| It says | Means | Do |
|---|---|---|
| *do not retry; this needs a different credential or a person* | your role may not, or policy/budget refused | ask, do not retry |
| *rate limited; wait before retrying* | too many calls for this identity | wait |
| *the connector's authorisation has expired or was revoked* | your Access session ended | reconnect the connector |

A budget refusal is a normal answer, not a fault. The model can read it.

### Nothing appears in the tool list

Check the version the origin is running and that `/mcp` is served:

```bash
scripts/on.sh staging control 'curl -sS http://127.0.0.1:8080/version'
```

## Related

- [ADR-0028](../adr/0028-remote-mcp-via-access-managed-oauth.md) — why Access
  issues the token and Workgraph implements no OAuth
- [ADR-0025](../adr/0025-api-first-external-clients.md) — why the tool set is
  six tools and not the whole API
- `test/acceptance/mcp_remote.sh` — proves the transport end to end, given a
  token
