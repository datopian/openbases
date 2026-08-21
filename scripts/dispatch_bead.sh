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

BEAD="${1:?usage: dispatch_bead.sh <bead-id> [rig] [timeout-seconds]}"
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
else
  echo "!! ${SLICE} is absent; falling back to su, and the per-cell resource"
  echo "!! limits will NOT apply to these agents. Re-run the execution_cell role."
  run() {
    su -s /bin/bash -c "export PATH=/usr/local/bin:\$PATH HOME=$CELL
cd $CELL/town && $1" "wgcell_${CELL_NAME}" 2>&1
  }
fi

teardown() {
  echo "  tearing down..."
  run "gt down >/dev/null 2>&1"
  sleep 3
  pkill -u wgcell_oss -f claude 2>/dev/null
  pkill -u wgcell_oss -f "gt daemon" 2>/dev/null
  sleep 2
  echo "  agents still running: $(pgrep -u wgcell_oss -c -f claude 2>/dev/null || echo 0)"
}
trap teardown EXIT INT TERM

echo "Dispatching $BEAD to $RIG (deadline ${DEADLINE}s)"

run "gt dolt start >/dev/null 2>&1"
for _ in $(seq 1 12); do
  run "gt dolt status 2>/dev/null" | grep -qi running && break
  sleep 5
done
run "gt daemon start >/dev/null 2>&1"
sleep 8

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
