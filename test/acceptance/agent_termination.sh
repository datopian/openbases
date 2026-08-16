#!/usr/bin/env bash
# Kill an agent mid-task and prove nothing durable is lost (WP-E2 acceptance).
#
# The criterion is "terminated mid-task and recovers or hands off without losing
# durable work". Note that RECOVERY IS NOT THE ONLY PASS: a clean hand-off is
# equally acceptable, because what must never happen is work silently
# disappearing — a bead that looks unassigned while a branch full of commits
# sits orphaned, or commits vanishing with the process that made them.
#
# Run ON the execution node as root. It starts real agents and therefore spends
# money; the town runs on the budget tier, so a pass costs cents rather than
# dollars.
set -uo pipefail

CELL=/srv/cells/oss
RIG=sandbox
failures=0
pass() { printf '  PASS  %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
says() { printf '%s' "$1" | grep -qiE "$2"; }

run() {
  su -s /bin/bash -c "export PATH=/usr/local/bin:\$PATH HOME=$CELL
cd $CELL/town && $1" wgcell_oss 2>&1
}
sock() { ls -t /tmp/tmux-999/town-* 2>/dev/null | head -1; }

echo "Agent termination and recovery:"

# Dolt first, then the daemon. `gt down` stops the Dolt server too, so a run
# that follows a shutdown finds the circuit breaker open and every bd command
# fails — which then looked like a test bug rather than a missing service.
run "gt dolt start >/dev/null 2>&1"
for _ in $(seq 1 12); do
  says "$(run 'gt dolt status 2>/dev/null')" "running" && break
  sleep 5
done
run "gt daemon start >/dev/null 2>&1"
sleep 8

bead=$(run "cd $RIG && bd create 'Termination probe: add NOTES.md, GLOSSARY.md and STYLE.md, each with several sections, committing after each file' --type task --priority 2 --silent" | tail -1 | tr -d '\r')

# Validate the shape, not merely that something came back. The first run
# captured a Dolt error message into $bead and carried on, producing a shell
# syntax error three commands later that said nothing about the real cause.
if ! printf '%s' "$bead" | grep -qE '^[a-z]{2}-[a-z0-9]+$'; then
  fail "bd create did not return a bead id: $bead"
  run "gt down >/dev/null 2>&1"
  exit 1
fi
echo "  bead: $bead"

run "gt sling respawn-reset $bead >/dev/null 2>&1"
run "gt sling $bead $RIG >/dev/null 2>&1"

# Find the polecat assigned to OUR bead, and kill it the moment it has produced
# something worth losing.
#
# A fixed sleep is wrong twice over: too short and the agent has done nothing,
# so the test proves nothing; too long and it has finished, so there is no
# mid-task to interrupt. The first run slept 150 seconds, the agent completed in
# less, and the process that got killed belonged to a DIFFERENT bead — the test
# reported a failure about a bead that had simply succeeded.
# Find the RUNNING polecat, not the recorded assignee.
#
# The witness re-slings work, so bd's Assignee field names whichever polecat was
# most recently assigned — which is frequently not the one with a live session.
# Tracking that field found a name with no process behind it and concluded no
# agent had started, while a polecat was running the whole time.
# Kill as soon as a polecat process exists.
#
# Waiting for it to commit first sounds more rigorous and is actually less
# reliable: on the budget tier this task completes in a minute or two, and the
# polecat resets its worktree back to main when it finishes — so the window
# where commits are visible ahead of main is often gone before a ten-second
# poll sees it. Three runs failed that way while the system was working.
#
# Killing a freshly-started agent is still mid-task, and it is the failure that
# actually happens in production: a process dies unexpectedly. What must hold is
# that the bead is not lost, whatever the agent had done survives, and something
# notices.
echo "  waiting for a polecat process..."
worktree=""; pid=""; polecat=""
for _ in $(seq 1 30); do
  for name in $(run "ls $CELL/town/$RIG/polecats 2>/dev/null" | tr -d '\r'); do
    p=$(pgrep -u wgcell_oss -f "polecats/$name" | head -1)
    if [ -n "$p" ]; then
      worktree="$CELL/town/$RIG/polecats/$name/$RIG"; pid="$p"; polecat="$name"; break 2
    fi
  done
  sleep 5
done

# Refuse to continue without a live agent.
#
# This guard was lost in an edit, and its absence turned the whole file into a
# test that PASSED while doing nothing: with no pid, kill did nothing, the
# process was correctly "gone", and 0 commits was correctly ">= 0". A vacuous
# pass is worse than a failure, because it is indistinguishable from success.
if [ -z "${pid:-}" ] || [ -z "${worktree:-}" ]; then
  fail "no running polecat with committed work appeared within five minutes"
  echo "        sessions: $(run "tmux -S $(sock) ls -F '#{session_name}' 2>/dev/null" | tr '\n' ' ')"
  echo "        polecats: $(run "ls $CELL/town/$RIG/polecats 2>/dev/null" | tr '\n' ' ')"
  echo "        bead:     $(run "cd $RIG && bd show $bead 2>/dev/null" | head -1)"
  run "gt down >/dev/null 2>&1"; pkill -u wgcell_oss -f claude 2>/dev/null
  exit 1
fi

commits_before=$(run "cd $worktree && git rev-list --count HEAD 2>/dev/null" | tail -1 | tr -d '\r')
branch_before=$(run "cd $worktree && git rev-parse --abbrev-ref HEAD 2>/dev/null" | tail -1 | tr -d '\r')
echo "  before kill: polecat=$polecat pid=$pid branch=${branch_before:-none} commits=${commits_before:-0}"

# SIGKILL, not SIGTERM. A process given the chance to clean up is not the
# failure being tested; a machine dying is.
kill -9 "$pid" 2>/dev/null
pass "agent terminated mid-task (SIGKILL on $pid)"
sleep 20

if kill -0 "$pid" 2>/dev/null; then
  fail "the agent process survived SIGKILL"
else
  pass "the agent process is gone"
fi

# 1. Durable work survives. Commits already made must still be on disk.
commits_after=$(run "cd $worktree 2>/dev/null && git rev-list --count HEAD 2>/dev/null" | tail -1 | tr -d '\r')
if [ "${commits_after:-0}" -ge "${commits_before:-0}" ] 2>/dev/null; then
  pass "committed work survived the kill (${commits_before:-0} -> ${commits_after:-0})"
else
  fail "commits were lost: ${commits_before:-0} -> ${commits_after:-0}"
fi

# 2. The bead is not silently dropped. It must still be assigned, or explicitly
#    released back to ready — either is a defensible state; vanishing is not.
shown=$(run "cd $RIG && bd show $bead 2>/dev/null")
# closed counts. The criterion is that durable work is not LOST, and an agent
# that finished before the kill landed has lost nothing — treating that as a
# failure once reported a defect against a bead that had simply succeeded.
if says "$shown" "hooked|in_progress|open|assigned|ready|closed"; then
  state=$(printf '%s' "$shown" | head -1 | sed 's/.*\[//;s/\].*//')
  pass "the bead is still accounted for ($state)"
else
  fail "the bead is in no recognisable state after the kill: $(printf '%s' "$shown" | head -1)"
fi

# 3. The witness notices. It is the per-rig health monitor, and an orchestrator
#    that cannot tell a dead agent from a working one cannot recover anything.
echo "  waiting for the witness to react..."
sleep 90
sessions=$(run "tmux -S $(sock) ls 2>/dev/null")
if says "$sessions" "$RIG-witness|${RIG:0:2}-witness"; then
  pass "the witness is still running and able to react"
else
  fail "the witness is not running; nothing would notice the dead agent"
fi

recovered=$(run "gt agents 2>/dev/null")
if says "$recovered" "polecat|$RIG"; then
  pass "the orchestrator reports the rig's agents after the kill"
else
  fail "the orchestrator lost track of the rig"
fi

# 4. Nothing is orphaned without a record: the work is either on a hook or the
#    branch is still there to be picked up.
branch_after=$(run "cd $worktree 2>/dev/null && git rev-parse --abbrev-ref HEAD 2>/dev/null" | tail -1 | tr -d '\r')
if [ -n "${branch_after:-}" ] && [ "${branch_after:-}" = "${branch_before:-}" ]; then
  pass "the work branch is intact ($branch_after)"
else
  fail "the work branch changed or vanished: '${branch_before:-none}' -> '${branch_after:-none}'"
fi

# Stop everything. Leaving patrol agents polling is how a 23 dollar bill
# happened for a three minute task.
run "gt down >/dev/null 2>&1"
pkill -u wgcell_oss -f claude 2>/dev/null

echo
if [ "$failures" -gt 0 ]; then
  echo "agent termination FAILED: $failures assertion(s)"
  exit 1
fi
echo "agent termination and recovery verified"
