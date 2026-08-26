#!/usr/bin/env bash
# wg-vft acceptance: something outside the node notices when the node is gone.
#
#   scripts/with_secrets.sh staging bash test/acceptance/external_probe.sh
#
# WP-I1's monitor runs ON the control node and delivers alerts THROUGH the
# database. If the node is down or PostgreSQL is unreachable there is nothing
# left to notice and nothing to deliver with — ADR-0020 states that blind spot
# rather than hiding it, and this is the check that closes it.
#
# The induction touches the node, because stopping a service requires it. The
# DETECTION must not: everything asserted below is observed from outside, the
# same way the scheduled prober observes it.
#
# It stops the control API on staging for about a minute.
set -uo pipefail

ENVIRONMENT="${WG_ENV:-staging}"
CONTROL_HOST="${WG_CONTROL_HOST:-ssh-staging.openbases.com}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

pass=0; fail=0
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail+1)); }
note() { printf '        %s\n' "$1"; }

tofu_out() { bash "$ROOT/scripts/tofu.sh" "$ENVIRONMENT" output -raw "$1" 2>/dev/null; }

HOSTNAME="$(tofu_out hostname)"
CLIENT_ID="$(tofu_out probe_client_id)"
CLIENT_SECRET="$(tofu_out probe_client_secret)"
if [ -z "$HOSTNAME" ] || [ -z "$CLIENT_ID" ] || [ -z "$CLIENT_SECRET" ]; then
  echo "no probe credentials in the OpenTofu state for $ENVIRONMENT." >&2
  echo "Apply the infrastructure first: scripts/tofu.sh $ENVIRONMENT apply" >&2
  exit 2
fi
URL="https://${HOSTNAME}/health/ready"

SSH=(ssh -o ProxyCommand="cloudflared access ssh --hostname %h"
     -o StrictHostKeyChecking=no -o ConnectTimeout=30 "root@${CONTROL_HOST}")

# The probe, exactly as the scheduled job makes it.
probe() {  # -> "<code> <body>"
  local body code
  body="$(mktemp)"
  code="$(curl -sS -o "$body" -w '%{http_code}' --max-time 20 \
            -H "CF-Access-Client-Id: ${CLIENT_ID}" \
            -H "CF-Access-Client-Secret: ${CLIENT_SECRET}" \
            "$URL" 2>/dev/null || echo 000)"
  printf '%s %s' "$code" "$(head -c 200 "$body" | tr -d '\n')"
  rm -f "$body"
}

restore() {
  "${SSH[@]}" "systemctl start control-api.service" >/dev/null 2>&1 || true
}
trap restore EXIT INT TERM

echo "wg-vft external probe acceptance  ($URL)"
echo

# ---------------------------------------------------------------------------
# 1. The naive probe is theatre, which is why the token exists
# ---------------------------------------------------------------------------
echo "== why a token is needed"
naive="$(curl -sSL -o /dev/null -w '%{http_code}' --max-time 25 "$URL" 2>/dev/null || echo 000)"
if [ "$naive" = "200" ]; then
  ok "an unauthenticated probe reports 200 — from the Access LOGIN PAGE, not the node"
  note "a health check written that way passes with the machine switched off"
else
  note "an unauthenticated probe returned $naive"
  ok "recorded"
fi

# ---------------------------------------------------------------------------
# 2. The real probe, against a healthy node
# ---------------------------------------------------------------------------
echo
echo "== healthy"
read -r code body <<<"$(probe)"
if [ "$code" = "200" ] && printf '%s' "$body" | grep -q '"status":"ready"'; then
  ok "the probe reaches the node and it reports ready"
else
  bad "expected 200/ready, got $code $body"
  echo; printf 'passed %d, failed %d\n' "$pass" "$fail"; exit 1
fi

# ---------------------------------------------------------------------------
# 3. Stop the API. Nothing on the node participates in noticing.
# ---------------------------------------------------------------------------
echo
echo "== with the control API stopped"
"${SSH[@]}" "systemctl stop control-api.service" >/dev/null 2>&1
sleep 5

read -r code body <<<"$(probe)"
if [ "$code" != "200" ]; then
  ok "the probe reports the outage from outside the node: HTTP $code"
  note "${body:-no body}"
else
  bad "the probe still reported 200 with the API stopped — it is not reaching the origin"
fi

# A redirect here would mean Access answered at the edge and the probe never
# reached the node, which is the failure this whole design exists to avoid. It
# must be a connection failure or a gateway error, not a login page.
case "$code" in
  301|302|303|307|308)
    bad "Access answered with a redirect, so the probe is not authenticating and never reaches the origin"
    ;;
  *)
    ok "the answer came from the origin path, not from an Access login redirect"
    ;;
esac

# ---------------------------------------------------------------------------
# 4. And it recovers
# ---------------------------------------------------------------------------
echo
echo "== recovery"
"${SSH[@]}" "systemctl start control-api.service" >/dev/null 2>&1
recovered=0
for _ in $(seq 1 12); do
  sleep 5
  read -r code body <<<"$(probe)"
  if [ "$code" = "200" ]; then recovered=1; break; fi
done
if [ "$recovered" = 1 ]; then
  ok "the probe clears once the API is back"
else
  bad "the probe did not recover; control-api may still be down — check the node"
fi

echo
printf 'passed %d, failed %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
