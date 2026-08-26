#!/usr/bin/env bash
# ADR-0023 acceptance: a bead dispatched to an agent is actually worked (wg-8el).
#
#   bash test/acceptance/agent_run.sh          # run from a WORKSTATION
#
# This is the claim Gas Town could not support on a headless node, and the one
# everything else was waiting on: a bead goes in, an agent reads it, does what it
# says, and the run leaves nothing behind.
#
# It spends real money — a few cents — because that is the only way to know. The
# budget gate bounds it, and the deadline bounds it again.
#
# From a workstation rather than a node, because the halves live on different
# machines: budgets are read on the control node, the agent runs on the execution
# node, and an execution node has no route to the control node except the control
# API through Access, by design.
set -uo pipefail

CELL_NAME="${WG_CELL:-oss}"
RIG="${WG_RIG:-sandbox}"
CONTROL_HOST="${WG_CONTROL_HOST:-ssh-staging.openbases.com}"
EXEC_HOST="${WG_EXEC_HOST:-ssh-exec-staging.openbases.com}"
CELL="/srv/cells/${CELL_NAME}"

pass=0; fail=0
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail+1)); }
note() { printf '        %s\n' "$1"; }

ACCESS_SSH=(-o ProxyCommand="cloudflared access ssh --hostname %h"
            -o StrictHostKeyChecking=no -o ConnectTimeout=30)
on_exec()    { ssh "${ACCESS_SSH[@]}" "root@${EXEC_HOST}" "$@"; }
on_control() { ssh "${ACCESS_SSH[@]}" "root@${CONTROL_HOST}" "$@"; }

# In the cell, as the cell user, inside its slice — the same way everything else
# reaches the rig's graph.
in_cell() {
  on_exec "systemd-run --quiet --pipe --collect --wait \
    --uid=wgcell_${CELL_NAME//-/_} --slice=wgcell-${CELL_NAME}.slice \
    --setenv=PATH=/usr/local/bin:/usr/bin:/bin --setenv=HOME=${CELL} \
    --working-directory=${CELL}/town/${RIG} /bin/bash -c $(printf '%q' "$1")"
}

echo "ADR-0023 agent run acceptance  (cell ${CELL_NAME}, rig ${RIG})"
echo

# ---------------------------------------------------------------------------
# 1. The rig's database is up, and stays up
# ---------------------------------------------------------------------------
echo "== the rig's graph"
if on_exec "systemctl is-active wg-rig-dolt-${CELL_NAME}.service" 2>/dev/null | grep -q '^active$'; then
  ok "the rig Dolt server is running as a managed unit"
  note "it used to be started by dispatch through a transient unit, which killed it immediately"
else
  bad "wg-rig-dolt-${CELL_NAME}.service is not active; no bead can be read"
  echo; printf 'passed %d, failed %d\n' "$pass" "$fail"; exit 1
fi

# ---------------------------------------------------------------------------
# 2. A real bead, with work in it that can be checked afterwards
# ---------------------------------------------------------------------------
echo
echo "== the bead"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
BEAD="$(in_cell "bd create 'Agent run acceptance ${STAMP}' --type task --priority 3 --silent \
  --description 'Add a comment containing exactly the word ACCEPTED-${STAMP} to this bead, then close it. Change no files.'" \
  2>/dev/null | tr -d '\r' | tail -1)"
if [ -z "$BEAD" ]; then
  bad "could not create a bead in the rig's graph"
  echo; printf 'passed %d, failed %d\n' "$pass" "$fail"; exit 1
fi
ok "created $BEAD"

cleanup() {
  in_cell "bd delete $BEAD --force" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------
# 3. Dispatch it
# ---------------------------------------------------------------------------
echo
echo "== the run"
on_exec "mkdir -p /var/tmp/wg-agent-acc/scripts" >/dev/null 2>&1
scp "${ACCESS_SSH[@]}" -q "$(dirname "$0")/../../scripts/dispatch_bead.sh" \
    "root@${EXEC_HOST}:/var/tmp/wg-agent-acc/scripts/dispatch_bead.sh" || {
  echo "could not copy the dispatcher" >&2; exit 2; }

OUT="$(on_exec "cd /var/tmp/wg-agent-acc && CELL_NAME=${CELL_NAME} timeout 400 bash scripts/dispatch_bead.sh $BEAD $RIG 240" 2>&1)"
printf '%s\n' "$OUT" | sed 's/^/        /' | tail -6

if printf '%s' "$OUT" | grep -q "budget:"; then
  ok "the budget was checked before anything was spent"
else
  bad "no budget check ran"
fi
if printf '%s' "$OUT" | grep -q "agent finished"; then
  ok "the agent ran to completion"
else
  bad "the agent did not finish"
fi

# ---------------------------------------------------------------------------
# 4. Did it actually do the work? Asked of the graph, not of the agent.
# ---------------------------------------------------------------------------
echo
echo "== the work"
SHOWN="$(in_cell "bd show $BEAD" 2>/dev/null)"
if printf '%s' "$SHOWN" | grep -q "ACCEPTED-${STAMP}"; then
  ok "the agent left the comment it was asked for"
else
  bad "the comment is not on the bead; the run did nothing"
  printf '%s\n' "$SHOWN" | sed 's/^/        /' | head -8
fi
if printf '%s' "$SHOWN" | grep -qi "CLOSED"; then
  ok "the agent closed the bead"
else
  bad "the bead is still open"
fi

# ---------------------------------------------------------------------------
# 5. Spend reached the bead
# ---------------------------------------------------------------------------
echo
echo "== the money"
on_control "systemctl start wg-costimport.service" >/dev/null 2>&1
sleep 10
SPEND="$(on_control "su - postgres -c \"psql -qAt -d workgraph -c \\\"SELECT round(sum(cost_cents),4) || ' cents, ' || count(*) || ' request(s), role=' || coalesce(max(role),'none') FROM usage_records WHERE bead = '$BEAD'\\\"\"" 2>/dev/null | tr -d '\r')"
if printf '%s' "$SPEND" | grep -qE "^[0-9]"; then
  ok "the run's spend arrived attributed to $BEAD"
  note "$SPEND"
else
  bad "no spend recorded against $BEAD; attribution is broken"
fi

# ---------------------------------------------------------------------------
# 6. Nothing left behind
# ---------------------------------------------------------------------------
echo
echo "== teardown"
if on_exec "ls -d ${CELL}/runs/$BEAD 2>/dev/null" >/dev/null 2>&1; then
  bad "the run directory is still there"
else
  ok "the run directory was removed"
fi
LEFT="$(on_exec "pgrep -u wgcell_${CELL_NAME//-/_} -c -f claude 2>/dev/null || echo 0" | tr -d '\r' | head -1)"
if [ "${LEFT:-0}" = "0" ]; then
  ok "no agent processes left running"
else
  bad "$LEFT agent process(es) still running"
fi
TRUST="$(on_exec "python3 -c \"import json;d=json.load(open('${CELL}/.claude.json'));print('yes' if '${CELL}/runs/$BEAD' in d.get('projects',{}) else 'no')\"" 2>/dev/null | tr -d '\r')"
if [ "$TRUST" = "no" ]; then
  ok "the workspace trust entry was removed"
else
  bad "a trust entry for this run is still in the cell's config"
fi

echo
printf 'passed %d, failed %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
