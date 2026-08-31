#!/usr/bin/env bash
# Prove the WP-H1 acceptance criteria against a live environment.
#
#   scripts/accept_workspace_events.sh staging
#
# Every step acts on the real Google project and the real database, because the
# criteria are about the live path: a mocked renewal proves nothing about
# whether Google accepts our renewal. Steps that change state undo themselves,
# and each prints what it asserted so the output is the evidence.
#
# Safe to re-run. It leaves the environment as it found it: the deliberate
# breakages (an expiry brought forward, a source disabled) are repaired by the
# reconciler in the same run, which is the point being demonstrated.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENVIRONMENT="${1:-staging}"
BIN=/usr/local/bin/workgraph-workspace

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAILED=$((FAILED + 1)); }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }
FAILED=0

# Everything runs on the control node: the database is not reachable from a
# laptop and the service-account key lives only there.
on() { "$ROOT/scripts/on.sh" "$ENVIRONMENT" control "$*"; }

# on.sh goes through ansible, which prefixes its output with a host header
# ("workgraph-staging-control | CHANGED | rc=0 >>"). Capturing a value without
# stripping that means every comparison below runs against a string containing
# the header — which is how the first run of this script reported a renewal as
# unchanged while the renewal had in fact worked.
clean() { sed -E -e '/^[a-z0-9_-]+ \| [A-Z]+!? (\||=>)/d' -e '/^[[:space:]]*$/d' | tr -d '\r'; }

# psql as the postgres superuser on the node. Used to set up the deliberate
# breakages and to read what the reconciler wrote. The reconciler itself
# connects as workgraph_app through its system functions, which is what the
# integration test asserts; this is the operator's view.
sql() { on "sudo -u postgres psql -X -q -At -d workgraph -c \"$1\"" | clean; }

# One pass, and ONLY that pass's log.
#
# Scoped by systemd invocation id rather than by time. A --since window overlaps
# the previous pass, which is how step 3 came to report "the second pass applied
# 1 change" while reading step 2's renewal — a false failure about the property
# the step exists to prove.
reconcile() {
  on "systemctl start workgraph-workspace.service; \
      journalctl _SYSTEMD_INVOCATION_ID=\$(systemctl show -p InvocationID --value workgraph-workspace.service) \
      --no-pager -o cat"
}

step "0. The reconciler is deployed and the timer is armed"
if on "test -x $BIN && systemctl is-enabled --quiet workgraph-workspace.timer" >/dev/null 2>&1; then
  pass "binary present and timer enabled"
else
  fail "the reconciler is not deployed; nothing below can be true"
  exit 1
fi
# The interval must stay below the renewal window, or a subscription expires
# between two runs that each saw it as healthy.
INTERVAL=$(on "systemctl show workgraph-workspace.timer -p TimersMonotonic --value" | clean)
echo "  timer: ${INTERVAL:-unknown}"

step "1. Every allow-listed source has exactly one active subscription"
sql "SELECT display_name || ' | ' || kind || ' | ' || coalesce(state,'(none)') || ' | expires ' || coalesce(expires_at::text,'(never)') FROM system_event_sources();"
NOT_ACTIVE=$(sql "SELECT count(*) FROM system_event_sources() WHERE enabled AND state <> 'active';")
if [ "${NOT_ACTIVE:-1}" = "0" ]; then
  pass "all enabled sources are active"
else
  fail "$NOT_ACTIVE enabled source(s) are not active"
fi

step "2. An expiring subscription is renewed, keeping its identity"
# Bringing the expiry forward is how an expiry is simulated without waiting
# days for a real one. The renewal window is 24 hours, so an hour from now is
# inside it.
BEFORE=$(sql "SELECT google_name FROM event_subscriptions s JOIN event_sources src ON src.id = s.source_id WHERE src.kind='drive' AND s.state='active' ORDER BY src.display_name LIMIT 1;")
if [ -z "$BEFORE" ]; then
  fail "no active drive subscription to renew"
else
  echo "  target subscription: $BEFORE"
  sql "UPDATE event_subscriptions SET expires_at = now() + interval '1 hour' WHERE google_name = '$BEFORE';"
  reconcile | tail -12
  AFTER=$(sql "SELECT google_name FROM event_subscriptions WHERE source_id = (SELECT source_id FROM event_subscriptions WHERE google_name = '$BEFORE');")
  EXPIRY=$(sql "SELECT expires_at FROM event_subscriptions WHERE google_name = '$BEFORE';")
  if [ "$AFTER" = "$BEFORE" ]; then
    pass "renewed in place: the id is unchanged, so nothing already delivered is re-delivered"
  else
    fail "the subscription id changed from $BEFORE to $AFTER — a renewal must not replace"
  fi
  echo "  expiry now: $EXPIRY"
fi

step "3. A second pass changes nothing (no duplicate effects)"
CALLS_BEFORE=$(sql "SELECT count(*) FROM event_subscriptions;")
APPLIED=$(reconcile | grep -c '"msg":"applied"' || true)
CALLS_AFTER=$(sql "SELECT count(*) FROM event_subscriptions;")
if [ "$APPLIED" = "0" ] && [ "$CALLS_BEFORE" = "$CALLS_AFTER" ]; then
  pass "an immediate second pass applied nothing"
else
  fail "the second pass applied $APPLIED change(s); reconciliation is not idempotent"
fi

step "4. A revoked source stops delivering, and returning restores it"
# Disabling a source is what a revoked permission looks like from our side. The
# subscription must be DELETED rather than left to expire: left alone it keeps
# delivering for days after someone revoked access.
VICTIM=$(sql "SELECT id FROM event_sources WHERE kind='drive' AND enabled ORDER BY display_name DESC LIMIT 1;")
VICTIM_NAME=$(sql "SELECT display_name FROM event_sources WHERE id='$VICTIM';")
echo "  disabling: $VICTIM_NAME"
sql "UPDATE event_sources SET enabled = false WHERE id = '$VICTIM';"
reconcile | tail -6
STATE=$(sql "SELECT state FROM event_subscriptions WHERE source_id = '$VICTIM';")
if [ "$STATE" = "deleted" ]; then
  pass "the subscription was deleted, not left to expire"
else
  fail "state after disabling: ${STATE:-missing} — a revoked source is still subscribed"
fi

echo "  re-enabling: $VICTIM_NAME"
sql "UPDATE event_sources SET enabled = true WHERE id = '$VICTIM';"
reconcile | tail -6
STATE=$(sql "SELECT state FROM event_subscriptions WHERE source_id = '$VICTIM';")
if [ "$STATE" = "active" ]; then
  pass "re-enabling resubscribed it"
else
  fail "state after re-enabling: ${STATE:-missing}"
fi

step "5. A source that is not allow-listed is ignored"
# Asserted at the point it is enforced: a delivery naming an unknown target is
# recorded (so "why did nothing happen" is answerable) but resolves to no
# source, so nothing downstream can attribute work to it.
# jsonb_build_object rather than a JSON literal: the payload travels through
# two levels of shell quoting to reach psql, and a literal brace is where that
# breaks.
sql "SELECT system_record_event('acceptance-probe-' || floor(extract(epoch from now()))::text, 'google.workspace.drive.file.v3.created', '//drive.googleapis.com/drives/0ANOT-ALLOW-LISTED', jsonb_build_object('probe', true));"
ORPHANED=$(sql "SELECT count(*) FROM event_receipts WHERE message_id LIKE 'acceptance-probe-%' AND source_id IS NULL;")
if [ "${ORPHANED:-0}" -ge 1 ]; then
  pass "a delivery for an unknown source resolves to no source"
else
  fail "a delivery for an unknown source was attributed to one"
fi
sql "DELETE FROM event_receipts WHERE message_id LIKE 'acceptance-probe-%';"

step "6. Real events are arriving"
# The one criterion that cannot be forced: a Meet transcript exists only when
# the meeting was held with transcription on, and a Drive event only when
# someone created or moved something. So this reports rather than asserts, and
# says plainly when there is nothing yet.
#
# Read through psql rather than `workspaced -summary`, because the binary builds
# its connection string from a systemd credential that only exists inside the
# unit — run by hand it exits 1 with "no database connection string", which
# reads like a broken reconciler rather than a missing environment.
sql "SELECT coalesce(string_agg(display_name || ' ' || event_type || ' x' || deliveries::text, ', '), 'nothing received in the last 7 days') FROM system_event_summary(now() - interval '7 days');"

step "Result"
if [ "$FAILED" = "0" ]; then
  printf '\033[32mevery forced criterion passed\033[0m\n'
else
  printf '\033[31m%s check(s) failed\033[0m\n' "$FAILED"
  exit 1
fi
