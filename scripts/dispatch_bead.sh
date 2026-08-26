#!/usr/bin/env bash
# Dispatch one bead to one agent, and stop.
#
# This used to bring a Gas Town up, sling the bead, and tear the town down. It
# now runs wg-runner (ADR-0023). Gas Town could not start an agent on a headless
# node at all: it revives a process in a tmux pane that already exists and never
# creates the session, so on a machine nobody has sat at, every agent fails.
#
# The original reason for this script survives the change and is worth keeping in
# view. Gas Town's patrol roles — Mayor, Deacon, witness, refinery — are
# persistent sessions that poll, and they cost money for as long as the town is
# up whether or not any work is happening: on one day of testing they made 1,616
# requests and $13.13 of a $20.92 bill, MORE than the work itself. Not starting
# them at all is a stronger version of not leaving them running.
#
# What remains is a budget check and one agent. The teardown that used to be a
# trap here now lives inside wg-runner, which registers it before it creates
# anything and runs it on every path including a signal — a teardown that only
# runs on the happy path leaves an agent running exactly when something has gone
# wrong and nobody is watching.
#
# Run ON the execution node as root.
set -uo pipefail

BEAD="${1:?usage: dispatch_bead.sh <bead-id> [rig] [timeout-seconds] [--no-budget-check]}"
RIG="${2:-sandbox}"
DEADLINE="${3:-900}"
CELL_NAME="${CELL_NAME:-oss}"
CELL="/srv/cells/${CELL_NAME}"
SLICE="wgcell-${CELL_NAME}.slice"

# The agent runs inside the cell's slice, not in whatever cgroup this script
# happens to be in.
#
# This used to be a plain `su`, and that was a hole rather than a detail. `su`
# from a root SSH session leaves the process in ROOT'S session scope, so every
# agent ran outside the per-cell cgroup and no CPU, memory or task limit applied
# to any of them: user-<uid>.slice carried a quota and held zero processes
# (WP-I3).
if [ ! -f "/etc/systemd/system/${SLICE}" ]; then
  echo "!! ${SLICE} is absent, so the per-cell CPU and memory limits do not exist."
  echo "!! Refusing rather than running an agent with no ceiling. Re-run the"
  echo "!! execution_cell role."
  exit 1
fi

# ---------------------------------------------------------------------------
# Budget: ask before spending, not after (wg-qw1)
# ---------------------------------------------------------------------------
#
# The control plane answers, not this script. The spend it compares against
# lives in the control database, and giving an execution node a database
# credential to read it would be a much larger grant than the question deserves.
# The cell already holds an Access service token; this uses it on a third narrow
# application that only answers this one question.
#
# --no-budget-check exists and is deliberately awkward to type. There are real
# reasons to override — the importer is down, or the work is an incident
# response — and an override that has to be argued for in the moment gets
# replaced by commenting out the check.
BUDGET_CHECK="${WG_BUDGET_CHECK:-1}"
[ "${4:-}" = "--no-budget-check" ] && BUDGET_CHECK=0

if [ "$BUDGET_CHECK" = 1 ]; then
  CREDS="$CELL/.credentials/control-api.env"
  if [ -r "$CREDS" ]; then
    # shellcheck disable=SC1090
    . "$CREDS"
    budget_json="$(curl -fsS --max-time 20 \
      -H "CF-Access-Client-Id: ${WG_ACCESS_CLIENT_ID:-}" \
      -H "CF-Access-Client-Secret: ${WG_ACCESS_CLIENT_SECRET:-}" \
      "${WG_CONTROL_API:-}/v1/budget/check?bead=${BEAD}&cell=${CELL_NAME}" 2>&1)" || budget_json=""

    if [ -z "$budget_json" ]; then
      # Reaching the control plane is part of being allowed to spend. Failing
      # open here would mean the budget silently stops existing exactly when
      # the platform is unwell, which is the least convenient moment to
      # discover that it had.
      echo "!! the budget check could not be reached; refusing to dispatch $BEAD"
      echo "!! override with: $0 $BEAD $RIG $DEADLINE --no-budget-check"
      exit 1
    fi

    allow="$(printf '%s' "$budget_json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("allow"))' 2>/dev/null || echo "")"
    reason="$(printf '%s' "$budget_json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("reason",""))' 2>/dev/null || echo "")"
    warning="$(printf '%s' "$budget_json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("warning",""))' 2>/dev/null || echo "")"

    [ -n "$warning" ] && echo "!! $warning"

    # The operational limits the governing budget carries (wg-726). They were
    # stored from the day budget_limits existed and read by nothing: the only
    # runtime ceiling in force was one node-wide systemd timer, and concurrency
    # was not enforced at all.
    MAX_AGENTS="$(printf '%s' "$budget_json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("max_agents") or 0)' 2>/dev/null || echo 0)"
    MAX_RUNTIME="$(printf '%s' "$budget_json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("max_runtime_minutes") or 0)' 2>/dev/null || echo 0)"
    if [ "$allow" != "True" ]; then
      echo "!! REFUSED: $reason"
      echo "!! raise it with: wg-budget set bead $BEAD <cents>   (on the control node)"
      echo "!! or override:   $0 $BEAD $RIG $DEADLINE --no-budget-check"
      exit 1
    fi
    echo "  budget: $reason"
  else
    echo "!! no control-api credentials at $CREDS; the budget cannot be checked"
    echo "!! override with: $0 $BEAD $RIG $DEADLINE --no-budget-check"
    exit 1
  fi
else
  echo "!! budget check SKIPPED for $BEAD"
  # No budget consulted means no limits to enforce. Stated rather than left to
  # an unset variable, because --no-budget-check already loosens one control and
  # should not silently loosen two more by accident.
  MAX_AGENTS=0
  MAX_RUNTIME=0
fi

# ---------------------------------------------------------------------------
# Run it
# ---------------------------------------------------------------------------
#
# wg-runner plans the run and executes it: a working directory under the cell, a
# settings file AT THE DEPTH THE AGENT RUNS — the thing Gas Town got wrong, which
# cost the bead tag and the model tier on every polecat for months (wg-7yo) — and
# a process with a deadline whose whole group is killed when it expires.
RUNNER="${WG_RUNNER:-/usr/local/bin/wg-runner}"
if [ ! -x "$RUNNER" ]; then
  echo "!! no runner at $RUNNER; deploy it with the execution_cell role"
  exit 1
fi

# The gateway token comes from the cell's own settings, which is where the
# gastown role put it. Read here rather than passed in, so a dispatch cannot run
# with a different one than the cell's own agents use.
TOKEN="$(python3 -c 'import json,re,sys
s = json.load(open(sys.argv[1]))
h = s.get("env", {}).get("ANTHROPIC_CUSTOM_HEADERS", "")
m = re.search(r"cf-aig-authorization: Bearer (\S+)", h)
print(m.group(1) if m else "")' "$CELL/.claude/settings.json" 2>/dev/null || true)"

if [ -z "$TOKEN" ]; then
  # Refused rather than run without it. Without the gateway a run does not fail
  # — it succeeds straight against the provider, untagged, unmetered and outside
  # the budget that was just checked.
  echo "!! no AI Gateway token in $CELL/.claude/settings.json"
  echo "!! refusing: a run without it escapes the budget checked above"
  exit 1
fi

# The budget's runtime ceiling wins when it is tighter than the argument.
#
# Tighter only, never looser: the argument is what a person asked for and the
# budget is what the project agreed to, so the smaller of the two is the honest
# answer. A budget that could RAISE the ceiling would let a forgotten row
# override an operator standing at the terminal.
if [ "${MAX_RUNTIME:-0}" -gt 0 ]; then
  budget_seconds=$(( MAX_RUNTIME * 60 ))
  if [ "$budget_seconds" -lt "$DEADLINE" ]; then
    echo "  runtime ceiling: ${MAX_RUNTIME}m from the budget, tighter than the ${DEADLINE}s requested"
    DEADLINE="$budget_seconds"
  fi
fi

echo "Dispatching $BEAD to $RIG (deadline ${DEADLINE}s)"

systemd-run --quiet --pipe --collect --wait \
  --uid="wgcell_${CELL_NAME}" --slice="$SLICE" \
  --setenv=PATH=/usr/local/bin:/usr/bin:/bin \
  --setenv=WG_AI_GATEWAY_TOKEN="$TOKEN" \
  --working-directory="$CELL" \
  "$RUNNER" \
    -bead "$BEAD" \
    -cell "$CELL_NAME" \
    -rig "$RIG" \
    -cell-root "$CELL" \
    -instructions "Work the bead $BEAD. Read it first, do what it asks, and close it when the work is done and not before. If you cannot complete it, leave it open and say why in a comment." \
    -deadline "${DEADLINE}s" \
    -max-agents "${MAX_AGENTS:-0}"
rc=$?

if [ "$rc" -eq 0 ]; then
  echo "  $BEAD: agent finished"
else
  echo "  $BEAD: agent did not finish cleanly (exit $rc)"
fi
exit "$rc"
