#!/usr/bin/env bash
# wg-qw1 acceptance: an exceeded budget visibly stops dispatch.
#
#   sudo bash test/acceptance/budget_gate.sh          # run ON the execution node
#
# The layers beneath this are covered elsewhere and cheaply:
#   test/integration/budgets.sql   resolution and summation, in the database
#   internal/budget                the decision, including staleness
#   wg-budget on the control node  setting and reading, against real data
#
# What only this can cover is the link between them: an execution node asking the
# control plane through Cloudflare Access, and scripts/dispatch_bead.sh acting on
# the answer. Every step there — the service token, the Access application bound
# to that one path, the JSON the script parses — is untested by the others, and
# an untested gate is a gate that is not there.
#
# It sets a budget, drives the endpoint, and puts the budget back. It never
# slings an agent: the refusal happens before the sling, which is the point.
set -uo pipefail

CELL_NAME="${WG_CELL:-oss}"
CELL="/srv/cells/${CELL_NAME}"
BEAD="${WG_BUDGET_TEST_BEAD:-wg-budget-gate-probe}"
CONTROL_HOST="${WG_CONTROL_HOST:-ssh-staging.openbases.com}"

pass=0; fail=0
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail+1)); }
note() { printf '        %s\n' "$1"; }

CREDS="$CELL/.credentials/control-api.env"
[ -r "$CREDS" ] || { echo "no control-api credentials at $CREDS" >&2; exit 2; }
# shellcheck disable=SC1090
. "$CREDS"

check() {  # bead -> the raw JSON
  curl -fsS --max-time 20 \
    -H "CF-Access-Client-Id: ${WG_ACCESS_CLIENT_ID:-}" \
    -H "CF-Access-Client-Secret: ${WG_ACCESS_CLIENT_SECRET:-}" \
    "${WG_CONTROL_API:-}/v1/budget/check?bead=$1&cell=${CELL_NAME}" 2>&1
}
field() { printf '%s' "$1" | python3 -c "import json,sys;print(json.load(sys.stdin).get('$2',''))" 2>/dev/null; }

# The budget is set on the CONTROL node, which is where wg-budget and the
# database are. Doing it over ssh keeps this one script rather than two halves
# somebody has to run in the right order.
SSH=(ssh -o ProxyCommand="cloudflared access ssh --hostname %h"
     -o StrictHostKeyChecking=no -o ConnectTimeout=30 "root@${CONTROL_HOST}")
set_budget() {  # kind key cents
  "${SSH[@]}" "systemd-run --quiet --pipe --wait --collect --unit=wg-budget-acc\$\$ \
    --property=Type=oneshot --property=User=workgraph \
    --property=EnvironmentFile=/etc/workgraph/control-api.env \
    --property=LoadCredential=db_app_password:/etc/workgraph/credentials/db_app_password \
    /usr/local/bin/wg-budget set $1 $2 $3" 2>&1
}

echo "wg-qw1 budget gate acceptance  (cell ${CELL_NAME})"
echo

# ---------------------------------------------------------------------------
# 1. The endpoint answers at all
# ---------------------------------------------------------------------------
echo "== the endpoint"
json="$(check "$BEAD")"
if printf '%s' "$json" | python3 -c 'import json,sys; json.load(sys.stdin)' 2>/dev/null; then
  ok "the execution node can reach the budget check through Access"
  note "$(field "$json" reason)"
else
  bad "the budget check did not answer with JSON"
  note "$json"
  note "if this is a 403, the Access application for /v1/budget/check does not exist yet:"
  note "  scripts/with_secrets.sh staging scripts/tofu.sh staging apply"
  echo; printf 'passed %d, failed %d\n' "$pass" "$fail"; exit 1
fi

# ---------------------------------------------------------------------------
# 2. A generous budget allows
# ---------------------------------------------------------------------------
echo
echo "== a budget with room"
set_budget cell "$CELL_NAME" 100000 >/dev/null
json="$(check "$BEAD")"
if [ "$(field "$json" allow)" = "True" ]; then
  ok "a budget with room allows dispatch"
  note "$(field "$json" reason)"
else
  bad "a budget with room refused: $(field "$json" reason)"
fi

# ---------------------------------------------------------------------------
# 3. A spent budget refuses, and says which one and by how much
# ---------------------------------------------------------------------------
echo
echo "== a spent budget"
set_budget cell "$CELL_NAME" 0 >/dev/null
json="$(check "$BEAD")"
reason="$(field "$json" reason)"
if [ "$(field "$json" allow)" = "False" ]; then
  ok "a spent budget refuses"
  note "$reason"
else
  bad "a spent budget still allowed dispatch"
fi

# The refusal has to be actionable at the moment somebody reads it: which budget,
# and what was spent against what.
if printf '%s' "$reason" | grep -q "$CELL_NAME"; then
  ok "the refusal names the budget that stopped it"
else
  bad "the refusal does not say which budget stopped it: $reason"
fi

# ---------------------------------------------------------------------------
# 4. The dispatcher acts on it, and does not sling
# ---------------------------------------------------------------------------
echo
echo "== the dispatcher"
out="$(CELL_NAME="$CELL_NAME" timeout 120 bash "$(dirname "$0")/../../scripts/dispatch_bead.sh" \
        "$BEAD" sandbox 30 2>&1 || true)"
if printf '%s' "$out" | grep -q "REFUSED"; then
  ok "dispatch_bead.sh refused before slinging"
  note "$(printf '%s' "$out" | grep -m1 'REFUSED')"
else
  bad "dispatch_bead.sh did not refuse"
  printf '%s\n' "$out" | sed 's/^/        /' | head -12
fi

if printf '%s' "$out" | grep -qi "Dispatching\|gt sling"; then
  bad "dispatch got as far as slinging despite the refusal"
else
  ok "nothing was slung, so the refusal cost nothing"
fi

# The override has to work, or the gate becomes a thing people delete.
out="$(CELL_NAME="$CELL_NAME" timeout 30 bash "$(dirname "$0")/../../scripts/dispatch_bead.sh" \
        "$BEAD" sandbox 5 --no-budget-check 2>&1 || true)"
if printf '%s' "$out" | grep -q "budget check SKIPPED"; then
  ok "--no-budget-check overrides the gate and says so"
else
  bad "--no-budget-check did not override the gate"
fi

# ---------------------------------------------------------------------------
# Put it back
# ---------------------------------------------------------------------------
echo
set_budget cell "$CELL_NAME" 10000 >/dev/null
echo "restored the ${CELL_NAME} cell budget to 10000 cents/day"

echo
printf 'passed %d, failed %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
