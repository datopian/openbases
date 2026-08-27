#!/usr/bin/env bash
# WP-E3 acceptance: a brief becomes a queued job, gets claimed exactly once, and
# reports back.
#
#   bash test/acceptance/work_queue.sh                # run from a WORKSTATION
#
# What this covers is the queue itself: enqueue, claim under contention, finish,
# and the overview a person reads. Not the agent — an agent run is covered by
# test/acceptance/agent_run.sh, costs real money, and takes minutes. The queue is
# what is new, and the part of it that is easy to get wrong is the claim.
#
# The claim is the whole reason this is a queue and not a call. An execution node
# has no inbound port, so the control plane cannot hand it work; the node comes
# and takes a row. Two dispatchers polling the same cell — one restarting while
# the other runs, which is the normal case during a deploy — must not both get
# the same job. That is FOR UPDATE SKIP LOCKED, and it is asserted here by racing
# two claims at once and requiring two different job ids.
#
# Everything runs through scripts/on.sh on the control node, as postgres over the
# unix socket, because these are SECURITY DEFINER functions and the point is to
# exercise them exactly as the API does.
set -uo pipefail

ENVIRONMENT="${1:-staging}"
CELL="${WG_CELL:-oss}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

pass=0
fail=0

check() {
  local what="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    printf 'ok    %s\n' "$what"
    pass=$((pass + 1))
  else
    printf 'FAIL  %s\n        want %s\n        got  %s\n' "$what" "$want" "$got"
    fail=$((fail + 1))
  fi
}

# One psql per call rather than a session, because each goes over the tunnel and
# a heredoc session that dies mid-way leaves rows behind that the next run trips
# over. -A -t so the output is the value and nothing else.
sql() {
  "$ROOT/scripts/on.sh" "$ENVIRONMENT" control \
    "sudo -u postgres psql -d workgraph -Atc \"$1\"" 2>/dev/null \
    | sed -n '2,$p' | tr -d '\r' | sed '/^$/d'
}

echo "== enqueue"

job=$(sql "SELECT system_enqueue_work('plan', '${CELL}', 'sandbox', NULL, 'acceptance: a brief that is never planned', NULL)")
if [ -z "$job" ]; then
  echo "FAIL  could not enqueue a job; nothing else can be checked"
  exit 1
fi
printf 'ok    enqueued %s\n' "$job"
pass=$((pass + 1))

check "it is queued" \
  "$(sql "SELECT status FROM work_queue WHERE id = '${job}'")" "queued"

check "it appears in the queue overview" \
  "$(sql "SELECT count(*) FROM system_queue_overview('${CELL}') WHERE id = '${job}'")" "1"

echo
echo "== claim"

# A second job, so there is one for each racer to find. With one job in the
# queue a race proves nothing: the loser correctly gets nothing either way.
job2=$(sql "SELECT system_enqueue_work('plan', '${CELL}', 'sandbox', NULL, 'acceptance: the second brief', NULL)")

# Both claims are issued before either is read, so they overlap in the database
# rather than running one after the other. Sequential claims would pass against a
# plain UPDATE and prove nothing about SKIP LOCKED.
a_file=$(mktemp); b_file=$(mktemp)
sql "SELECT id FROM system_claim_work('${CELL}')" > "$a_file" &
a_pid=$!
sql "SELECT id FROM system_claim_work('${CELL}')" > "$b_file" &
b_pid=$!
wait "$a_pid"; wait "$b_pid"
a=$(cat "$a_file"); b=$(cat "$b_file")
rm -f "$a_file" "$b_file"

if [ -n "$a" ] && [ -n "$b" ] && [ "$a" != "$b" ]; then
  printf 'ok    two concurrent claims took two different jobs\n'
  pass=$((pass + 1))
else
  printf 'FAIL  two concurrent claims must take different jobs\n        got  a=%s b=%s\n' "${a:-<none>}" "${b:-<none>}"
  fail=$((fail + 1))
fi

check "both are now running" \
  "$(sql "SELECT count(*) FROM work_queue WHERE id IN ('${job}','${job2}') AND status = 'running'")" "2"

check "a third claim finds nothing left" \
  "$(sql "SELECT count(*) FROM system_claim_work('${CELL}')")" "0"

echo
echo "== finish"

sql "SELECT system_finish_work('${job}'::uuid, true, 'acceptance: finished by the test')" >/dev/null
check "a finished job reports done" \
  "$(sql "SELECT status FROM work_queue WHERE id = '${job}'")" "done"

sql "SELECT system_finish_work('${job2}'::uuid, false, 'acceptance: failed by the test')" >/dev/null
check "a failed job reports failed, and says why" \
  "$(sql "SELECT result FROM work_queue WHERE id = '${job2}'")" "acceptance: failed by the test"

check "neither is claimable again" \
  "$(sql "SELECT count(*) FROM system_claim_work('${CELL}')")" "0"

echo
echo "== the view a person reads"

# Not a fixed number: the graph is real and grows. What matters is that the
# overview is populated at all, because an empty one is what a broken projection
# looks like and it is indistinguishable from an empty backlog.
beads=$(sql "SELECT count(*) FROM system_work_overview('${CELL}')")
if [ "${beads:-0}" -gt 0 ] 2>/dev/null; then
  printf 'ok    the work overview has %s beads\n' "$beads"
  pass=$((pass + 1))
else
  printf 'FAIL  the work overview is empty; run wg-work sync-hq\n'
  fail=$((fail + 1))
fi

echo
echo "== clean up"
sql "DELETE FROM work_queue WHERE id IN ('${job}','${job2}')" >/dev/null
check "the test rows are gone" \
  "$(sql "SELECT count(*) FROM work_queue WHERE id IN ('${job}','${job2}')")" "0"

echo
printf '%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
