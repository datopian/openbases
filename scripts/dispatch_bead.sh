#!/usr/bin/env bash
# Dispatch one bead, wait for it, and always tear the town down.
#
# Why this exists rather than "run gt sling":
#
# Gas Town's patrol roles — Mayor, Deacon, witness, refinery — are persistent
# agent sessions that poll. They cost money for as long as the town is up,
# whether or not any work is happening. On one day of testing they made 1,616
# requests and 365,671 output tokens, which was $13.13 of a $20.92 bill: MORE
# than the work itself. Their polling interval is not exposed as configuration,
# so the only reliable control is not leaving the town running.
#
# The teardown is a trap, not a final line. A script that tears down only on the
# happy path leaves agents polling exactly when something went wrong and nobody
# is watching, which is the expensive case.
#
# Run ON the execution node as root.
set -uo pipefail

BEAD="${1:?usage: dispatch_bead.sh <bead-id> [rig] [timeout-seconds] [--no-budget-check]}"
RIG="${2:-sandbox}"
DEADLINE="${3:-900}"
CELL_DEFAULT_NAME="oss"
CELL=/srv/cells/${CELL_NAME:-$CELL_DEFAULT_NAME}

CELL_NAME="${CELL_NAME:-oss}"
SLICE="wgcell-${CELL_NAME}.slice"

# Agents run inside the cell's slice, not in whatever cgroup this script happens
# to be in.
#
# This used to be a plain `su`, and that was a hole rather than a detail. `su`
# from a root SSH session leaves the process in ROOT'S session scope, so every
# agent ran outside the per-cell cgroup and no CPU, memory or task limit applied
# to any of them. user-<uid>.slice carried a quota and held zero processes
# (WP-I3). systemd-run places the town — and therefore tmux, and therefore every
# agent tmux spawns — inside the limited slice.
#
# Falls back to `su` when the slice is absent, so a node that has not been
# re-provisioned still dispatches rather than failing; the fallback is announced
# because it means the limits are not in force.
if [ -f "/etc/systemd/system/${SLICE}" ]; then
  run() {
    systemd-run --quiet --pipe --collect --wait \
      --uid="wgcell_${CELL_NAME}" --slice="$SLICE" \
      --setenv=PATH=/usr/local/bin:/usr/bin:/bin \
      --setenv=HOME="$CELL" \
      --working-directory="$CELL/town" \
      /bin/bash -lc "$1" 2>&1
  }

  # Daemons need a unit of their own, and it must not be --wait --collect.
  #
  # `run` finishes when its command finishes, and systemd then tears down the
  # unit's whole cgroup — taking with it anything the command forked. So
  # `run "gt dolt start"` started a Dolt server and killed it a moment later:
  #
  #   ✓ Dolt server started (PID 2807261, port 3307)
  #   ...
  #   ○ Dolt server is not running
  #
  # After which `gt dolt status` never reports running, the sixty-second wait
  # below expires, and `gt sling` fails with a usage message — which reads like
  # a wrong argument rather than like a missing database. Dispatch could not
  # have worked on any node that has the slice, which is every node since WP-I3
  # introduced it; the `su` fallback below is what used to keep it alive.
  #
  # Type=forking, because that is exactly what these programs do: the process
  # systemd starts exits, and systemd tracks the child it left behind. The unit
  # persists until the teardown stops it, and it still runs inside the cell's
  # slice, so the CPU and memory limits apply to the daemon too.
  daemon() {  # name command
    systemd-run --quiet --unit="$1" --property=Type=forking \
      --uid="wgcell_${CELL_NAME}" --slice="$SLICE" \
      --setenv=PATH=/usr/local/bin:/usr/bin:/bin \
      --setenv=HOME="$CELL" \
      --working-directory="$CELL/town" \
      /bin/bash -lc "$2" 2>&1
  }
  stop_daemon() { systemctl stop "$1" >/dev/null 2>&1; systemctl reset-failed "$1" >/dev/null 2>&1; }
else
  echo "!! ${SLICE} is absent; falling back to su, and the per-cell resource"
  echo "!! limits will NOT apply to these agents. Re-run the execution_cell role."
  run() {
    su -s /bin/bash -c "export PATH=/usr/local/bin:\$PATH HOME=$CELL
cd $CELL/town && $1" "wgcell_${CELL_NAME}" 2>&1
  }
  # Without the slice there is no cgroup to be torn down, so a daemon started
  # this way simply survives.
  daemon()      { run "$2"; }
  stop_daemon() { :; }
fi

DOLT_UNIT="gt-dolt-${CELL_NAME}"
GT_DAEMON_UNIT="gt-daemon-${CELL_NAME}"

teardown() {
  echo "  tearing down..."
  # Untag first, while the town is still up and the files are still there. A
  # bead left in cf-aig-metadata would attribute the NEXT dispatch's spend to
  # this bead, and the budget that then trips would be the wrong one.
  run "wg-tag-bead $RIG --clear 2>&1 | tail -1" || true
  run "gt down >/dev/null 2>&1"
  # The daemons outlive their starting command by design now, so they have to be
  # stopped explicitly. Leaving them would leave a Dolt server and a Gas Town
  # daemon polling in the cell after every dispatch — which is the cost this
  # script exists to avoid.
  stop_daemon "$GT_DAEMON_UNIT"
  stop_daemon "$DOLT_UNIT"
  sleep 3
  pkill -u wgcell_oss -f claude 2>/dev/null
  pkill -u wgcell_oss -f "gt daemon" 2>/dev/null
  sleep 2
  echo "  agents still running: $(pgrep -u wgcell_oss -c -f claude 2>/dev/null || echo 0)"
}
trap teardown EXIT INT TERM

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
fi

echo "Dispatching $BEAD to $RIG (deadline ${DEADLINE}s)"

stop_daemon "$DOLT_UNIT"
daemon "$DOLT_UNIT" "gt dolt start" >/dev/null 2>&1
dolt_up=0
for _ in $(seq 1 12); do
  # Captured into a variable, then matched. NOT piped into `grep -q`.
  #
  # Two bugs met here and each hid the other. `gt dolt status` writes its answer
  # to STDERR, so the original `2>/dev/null` threw the answer away. And piping
  # into `grep -q` makes grep exit at the first match, which closes the pipe,
  # which SIGPIPEs systemd-run — and under `set -o pipefail` the pipeline then
  # reports 141, so a SUCCESSFUL match was read as a failure.
  #
  # That is the third time today the `grep -q` half has bitten: it also produced
  # "tar: stdout: write error" on a perfectly good archive in backup_offsite.sh.
  # The rule that survives: never pipe into `grep -q` under pipefail when the
  # left-hand side matters.
  dolt_status="$(run "gt dolt status 2>&1")"
  case "$dolt_status" in
    *"server is running"*) dolt_up=1; break ;;
  esac
  sleep 5
done
if [ "$dolt_up" != 1 ]; then
  # Said plainly rather than left to surface as a confusing `gt sling` usage
  # message sixty seconds later.
  echo "!! the rig's Dolt server did not come up; nothing can be slung"
  printf '%s\n' "$dolt_status" | sed 's/^/!! /' | head -4
  exit 1
fi
echo "  dolt: up"

stop_daemon "$GT_DAEMON_UNIT"
daemon "$GT_DAEMON_UNIT" "gt daemon start" >/dev/null 2>&1
sleep 8

# Tag every agent in the rig with this bead, so the spend it makes carries the
# bead into the gateway log and out again through the importer. Without this,
# usage_records knows the cell and the role and not the piece of work, and a
# per-bead budget has nothing to compare against.
echo "  $(run "wg-tag-bead $RIG $BEAD 2>&1 | tail -1")"

run "gt sling respawn-reset $BEAD >/dev/null 2>&1"
slung=$(run "gt sling $BEAD $RIG 2>&1 | tail -2")
echo "  $slung"

# Poll for completion. The bead closing is the signal, not the agent process
# exiting — a polecat can exit and be re-slung, and treating that as done would
# tear the town down mid-recovery.
start=$(date +%s)
while :; do
  shown=$(run "cd $RIG && bd show $BEAD 2>/dev/null" | head -1)
  case "$shown" in
    *CLOSED*) echo "  closed: $shown"; break ;;
  esac
  now=$(date +%s)
  if [ $((now - start)) -ge "$DEADLINE" ]; then
    echo "  DEADLINE reached after ${DEADLINE}s; tearing down with the bead still open"
    echo "  last state: $shown"
    exit 1
  fi
  sleep 15
done
