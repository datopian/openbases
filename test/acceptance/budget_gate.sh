#!/usr/bin/env bash
# wg-qw1 acceptance: an exceeded budget visibly stops dispatch.
#
#   bash test/acceptance/budget_gate.sh               # run from a WORKSTATION
#
# From a workstation, not from a node, because the two halves live on different
# machines and the nodes cannot reach each other. Budgets are set on the control
# node, where wg-budget and the database are; the check and the dispatch happen
# on the execution node. An execution node has no route to the control node
# except the control API through Access — deliberately — so a script that tried
# to do both from there failed with "Connection timed out during banner
# exchange", which reads like a network fault rather than like isolation working.
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
EXEC_HOST="${WG_EXEC_HOST:-ssh-exec-staging.openbases.com}"

ACCESS_SSH=(-o ProxyCommand="cloudflared access ssh --hostname %h"
            -o StrictHostKeyChecking=no -o ConnectTimeout=30)
on_control() { ssh "${ACCESS_SSH[@]}" "root@${CONTROL_HOST}" "$@"; }
on_exec()    { ssh "${ACCESS_SSH[@]}" "root@${EXEC_HOST}" "$@"; }

pass=0; fail=0
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail+1)); }
note() { printf '        %s\n' "$1"; }

command -v cloudflared >/dev/null || { echo "cloudflared is not installed" >&2; exit 2; }

# The check runs ON the execution node using the cell's own Access service token,
# because that is the path being tested: a cell asking the control plane. Curling
# it from here with a workstation identity would prove something else.
check() {  # bead -> the raw JSON
  on_exec ". $CELL/.credentials/control-api.env && curl -fsS --max-time 20 \
    -H \"CF-Access-Client-Id: \$WG_ACCESS_CLIENT_ID\" \
    -H \"CF-Access-Client-Secret: \$WG_ACCESS_CLIENT_SECRET\" \
    \"\$WG_CONTROL_API/v1/budget/check?bead=$1&cell=${CELL_NAME}\"" 2>&1
}
field()  { printf '%s' "$1" | python3 -c "import json,sys;print(json.load(sys.stdin).get('$2',''))" 2>/dev/null; }
# The subject that actually governs, which is NOT necessarily the one you would
# guess: resolution takes the most specific budget that exists, so setting a cell
# ceiling has no effect at all when the project has one. The first version of
# this test zeroed the cell, watched the project budget keep allowing, and
# reported a broken gate — the gate was right and the test was aimed wrong.
subject() { printf '%s' "$1" | python3 -c "import json,sys;s=json.load(sys.stdin).get('status',{});print(s.get('subject_kind',''),s.get('subject_key',''))" 2>/dev/null; }

# Budgets are set on the CONTROL node, which is where wg-budget and the database
# are. Through systemd-run so the tool gets the same environment and database
# credential the service does, rather than a hand-assembled one that could
# succeed where the real path fails.
set_budget() {  # kind key cents
  on_control "systemd-run --quiet --pipe --wait --collect --unit=wg-budget-acc-\$RANDOM \
    --property=Type=oneshot --property=User=workgraph \
    --property=EnvironmentFile=/etc/workgraph/control-api.env \
    --property=LoadCredential=db_app_password:/etc/workgraph/credentials/db_app_password \
    /usr/local/bin/wg-budget set $1 $2 $3" 2>&1
}

echo "wg-qw1 budget gate acceptance  (cell ${CELL_NAME})"
echo

# The dispatcher under test is the one in this checkout, not whatever happens to
# be on the node.
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
on_exec "mkdir -p /var/tmp/wg-acc/scripts" >/dev/null 2>&1
scp "${ACCESS_SSH[@]}" -q "$ROOT/scripts/dispatch_bead.sh" \
    "root@${EXEC_HOST}:/var/tmp/wg-acc/scripts/dispatch_bead.sh" || {
  echo "could not copy the dispatcher to ${EXEC_HOST}" >&2; exit 2; }

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
# 2. Which budget actually governs this bead
# ---------------------------------------------------------------------------
echo
echo "== the governing budget"
read -r KIND KEY <<<"$(subject "$json")"
if [ -n "$KIND" ] && [ "$KIND" != "none" ]; then
  ok "the governing budget is the $KIND budget for $KEY"
  note "resolution takes the most specific that exists: bead, else project, else cell"
else
  bad "no budget governs $BEAD, so there is nothing to test refusing"
  echo; printf 'passed %d, failed %d\n' "$pass" "$fail"; exit 1
fi
ORIGINAL="$(printf '%s' "$json" | python3 -c "import json,sys;print(json.load(sys.stdin)['status'].get('daily_cents','0'))" 2>/dev/null)"
note "current ceiling: $ORIGINAL cents/day"

# ---------------------------------------------------------------------------
# 3. A generous budget allows
# ---------------------------------------------------------------------------
echo
echo "== a budget with room"
set_budget "$KIND" "$KEY" 100000 >/dev/null
json="$(check "$BEAD")"
if [ "$(field "$json" allow)" = "True" ]; then
  ok "a budget with room allows dispatch"
  note "$(field "$json" reason)"
else
  bad "a budget with room refused: $(field "$json" reason)"
fi

# ---------------------------------------------------------------------------
# 4. A spent budget refuses, and says which one and by how much
# ---------------------------------------------------------------------------
echo
echo "== a spent budget"
set_budget "$KIND" "$KEY" 0 >/dev/null
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
if printf '%s' "$reason" | grep -q "$KEY"; then
  ok "the refusal names the budget that stopped it"
else
  bad "the refusal does not say which budget stopped it: $reason"
fi

# ---------------------------------------------------------------------------
# 5. The dispatcher acts on it, and does not sling
# ---------------------------------------------------------------------------
echo
echo "== the dispatcher"
out="$(on_exec "CELL_NAME=$CELL_NAME timeout 120 bash /var/tmp/wg-acc/scripts/dispatch_bead.sh $BEAD sandbox 30" 2>&1 || true)"
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
out="$(on_exec "CELL_NAME=$CELL_NAME timeout 60 bash /var/tmp/wg-acc/scripts/dispatch_bead.sh $BEAD sandbox 5 --no-budget-check" 2>&1 || true)"
if printf '%s' "$out" | grep -q "budget check SKIPPED"; then
  ok "--no-budget-check overrides the gate and says so"
else
  bad "--no-budget-check did not override the gate"
fi

# ---------------------------------------------------------------------------
# Put it back
# ---------------------------------------------------------------------------
echo
set_budget "$KIND" "$KEY" "${ORIGINAL:-10000}" >/dev/null
echo "restored the $KIND budget for $KEY to ${ORIGINAL:-10000} cents/day"

echo
printf 'passed %d, failed %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
