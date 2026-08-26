#!/usr/bin/env bash
# WP-I1 acceptance: each of the five failure classes triggers the correct alert,
# and the alert reaches an operator with a runbook link.
#
#   sudo bash test/acceptance/alerting.sh [path-to-wg-monitor]
#
# Every failure is INDUCED, not simulated. The monitor runs as it does in
# production — same binary, same code path, same delivery functions — against a
# database this script creates and drops. Nothing is mocked, and nothing touches
# the real staging database: an acceptance test that writes into people's real
# inboxes is one that gets run once.
#
# The seven inductions:
#
#   api          point the health probe at a closed port
#   service      declare a unit that does not exist
#   webhook      insert a delivery that has been unprocessed for two hours
#   agent stall  claim a cell is deployed and let the silence threshold expire
#   disk         set the threshold below the filesystem's real usage
#   backup       point the receipt directory at one with no receipts
#   cost import  declare a gateway that has never been imported
#
# Two of those are threshold moves rather than physically inducing the condition.
# Filling a real disk on a live node to test an alert would risk the outage the
# alert exists to prevent, and the threshold comparison is the code under test
# either way — the number on the other side of it is not.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MONITOR="${1:-$ROOT/bin/linux-amd64/monitor}"
DB="wg_alerting_$$"
PSQL_SUPER=(sudo -u postgres psql -v ON_ERROR_STOP=1 -q)

pass=0
fail=0

ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail + 1)); }
note() { printf '        %s\n' "$1"; }

[ -x "$MONITOR" ] || { echo "no monitor binary at $MONITOR (make build-linux)" >&2; exit 2; }
command -v psql >/dev/null || { echo "psql is not installed" >&2; exit 2; }

cleanup() {
  "${PSQL_SUPER[@]}" -c "DROP DATABASE IF EXISTS $DB" >/dev/null 2>&1
  rm -rf "$RECEIPTS" "$EMPTY_RECEIPTS" 2>/dev/null
}
RECEIPTS="$(mktemp -d)"
EMPTY_RECEIPTS="$(mktemp -d)"
trap cleanup EXIT INT TERM

echo "WP-I1 alerting acceptance"
echo

# ---------------------------------------------------------------------------
# A database of its own, with a password the monitor can actually use.
# ---------------------------------------------------------------------------
PW="acceptance-$$-$(date +%s)"
sudo -u postgres createdb "$DB" || { echo "could not create $DB" >&2; exit 2; }
for f in "$ROOT"/db/migrations/*.sql; do
  "${PSQL_SUPER[@]}" -d "$DB" -f "$f" >/dev/null || { echo "migration $f failed" >&2; exit 2; }
done
# workgraph_app is NOLOGIN by design, so it cannot connect. The monitor needs a
# real login, and it must NOT be a superuser: a superuser bypasses row-level
# security, and this test would then prove nothing about what the monitor can
# actually see in production.
"${PSQL_SUPER[@]}" -d "$DB" >/dev/null <<SQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wg_acceptance') THEN
    CREATE ROLE wg_acceptance LOGIN;
  END IF;
END \$\$;
ALTER ROLE wg_acceptance PASSWORD '$PW';
GRANT workgraph_app TO wg_acceptance;
SQL
export WG_DATABASE_URL="postgres://wg_acceptance:$PW@127.0.0.1:5432/$DB?sslmode=disable"

admins=$("${PSQL_SUPER[@]}" -d "$DB" -tAc \
  "SELECT count(*) FROM role_grants WHERE role_name='organisation_admin' AND project_id IS NULL")
if [ "${admins:-0}" -eq 0 ]; then
  echo "no organisation admin in the seed: every alert would reach nobody and" >&2
  echo "this test would pass while proving the opposite of what it claims" >&2
  exit 2
fi
note "$admins organisation admin(s) will receive alerts"
echo

# run <name> <extra monitor args...>; prints nothing, sets $OUT and $RC
run() {
  OUT="$("$MONITOR" "$@" 2>&1)"
  RC=$?
}

# alerted <check> -> is there an open inbox item for this check, with a runbook?
alerted() {
  "${PSQL_SUPER[@]}" -d "$DB" -tAc "
    SELECT count(*) FROM attention_items
     WHERE dedupe_key = 'monitor:$1'
       AND status = 'open'
       AND explanation->>'runbook' IS NOT NULL
       AND explanation->>'runbook' <> ''"
}

runbook_of() {
  "${PSQL_SUPER[@]}" -d "$DB" -tAc \
    "SELECT DISTINCT explanation->>'runbook' FROM attention_items WHERE dedupe_key='monitor:$1'"
}

# ---------------------------------------------------------------------------
# 1..7: induce each failure and require the matching alert to be delivered.
# ---------------------------------------------------------------------------

# Baseline the healthy conditions so each case changes exactly one thing.
date -u +%Y-%m-%dT%H:%M:%SZ > "$RECEIPTS/beads.receipt"
date -u +%Y-%m-%dT%H:%M:%SZ > "$RECEIPTS/postgres.receipt"
# A unit that certainly IS running, so the service check is green in the
# baseline. init is the one unit every Linux system has.
HEALTHY=(-receipts "$RECEIPTS" -backup-streams "beads:24h,postgres:24h"
         -disk-percent 100 -expect-cells 0
         -services "init.scope" -gateways ""
         -health-url "http://127.0.0.1:8080/health/ready")

echo "1. API unavailable"
run -health-url "http://127.0.0.1:1/health/ready" \
    -receipts "$RECEIPTS" -backup-streams "beads:24h,postgres:24h" \
    -disk-percent 100 -expect-cells 0
n=$(alerted api)
if [ "$n" = "$admins" ]; then ok "delivered to $n inbox(es), runbook $(runbook_of api)"
else bad "expected $admins alert(s), found $n"; note "$OUT"; fi

echo "2. A declared service is not running"
# A unit that does not exist rather than stopping a real one: the acceptance
# test must not take the platform down to prove it would notice. systemd reports
# an unknown unit as inactive, which is the same answer and the same code path.
run "${HEALTHY[@]}" -services "init.scope,wg-acceptance-absent.service"
n=$(alerted service)
if [ "$n" = "$admins" ]; then ok "delivered to $n inbox(es), runbook $(runbook_of service)"
else bad "expected $admins alert(s), found $n"; note "$OUT"; fi

echo "3. Webhook backlog"
"${PSQL_SUPER[@]}" -d "$DB" -c "
  INSERT INTO github_deliveries (delivery_id, event_type, received_at, processed_at, payload)
  VALUES ('acceptance-stuck', 'push', now() - interval '2 hours', NULL, '{}'::jsonb)" >/dev/null
run "${HEALTHY[@]}" -webhook-backlog-age 15m
n=$(alerted webhook)
if [ "$n" = "$admins" ]; then ok "delivered to $n inbox(es), runbook $(runbook_of webhook)"
else bad "expected $admins alert(s), found $n"; note "$OUT"; fi

echo "4. Agent stall (the witness has gone quiet)"
run "${HEALTHY[@]}" -expect-cells 1 -agent-health-silence 1s
n=$(alerted agent_stall)
if [ "$n" = "$admins" ]; then ok "delivered to $n inbox(es), runbook $(runbook_of agent_stall)"
else bad "expected $admins alert(s), found $n"; note "$OUT"; fi

echo "5. Disk pressure"
run "${HEALTHY[@]}" -disk-percent 1
n=$(alerted disk)
if [ "$n" = "$admins" ]; then ok "delivered to $n inbox(es), runbook $(runbook_of disk)"
else bad "expected $admins alert(s), found $n"; note "$OUT"; fi

echo "6. Backup stale"
run "${HEALTHY[@]}" -receipts "$EMPTY_RECEIPTS"
n=$(alerted backup)
if [ "$n" = "$admins" ]; then ok "delivered to $n inbox(es), runbook $(runbook_of backup)"
else bad "expected $admins alert(s), found $n"; note "$OUT"; fi

echo "7. Spend is no longer being imported"
# A gateway that has never been imported, which is the state a fresh install and
# a stopped importer share. Keyed off the last RUN and not the newest usage
# record: a gateway nobody has used has an old record however punctually the
# importer ran, so measuring the record would alert loudest on the quietest
# system — the mistake 0033 had to undo for the budget check.
run "${HEALTHY[@]}" -gateways "wg-acceptance-never-imported"
n=$(alerted cost_import)
if [ "$n" = "$admins" ]; then ok "delivered to $n inbox(es), runbook $(runbook_of cost_import)"
else bad "expected $admins alert(s), found $n"; note "$OUT"; fi
echo

# ---------------------------------------------------------------------------
# The properties that make the alerts usable rather than merely present.
# ---------------------------------------------------------------------------
echo "Alert quality"

# Each class alerted on its OWN rule. Checks that all raise one rule would have
# passed every assertion above.
#
# The expected count is derived from the inductions rather than written as a
# literal. The literal said five while seven classes existed, so adding a class
# left this assertion failing for a reason unrelated to the class that was added.
CLASSES=$(grep -cE '^echo "[0-9]+\. ' "$0")
rules=$("${PSQL_SUPER[@]}" -d "$DB" -tAc \
  "SELECT count(DISTINCT rule) FROM attention_items WHERE dedupe_key LIKE 'monitor:%'")
if [ "$rules" = "$CLASSES" ]; then ok "$rules distinct rules, one per induction"
else bad "expected $CLASSES distinct rules, found $rules"; fi

# Every runbook the operator was handed must exist in the repository. A link to a
# missing file satisfies the code and fails the person reading it at 3am.
missing=0
if [ ! -d "$ROOT/docs/runbooks" ]; then
  bad "no docs/runbooks in $ROOT: this check needs the repository, not just the binary"
  missing=1
fi
for rb in $("${PSQL_SUPER[@]}" -d "$DB" -tAc \
    "SELECT DISTINCT explanation->>'runbook' FROM attention_items WHERE dedupe_key LIKE 'monitor:%'"); do
  [ -d "$ROOT/docs/runbooks" ] || break
  [ -f "$ROOT/$rb" ] || { bad "runbook $rb does not exist"; missing=1; }
done
[ "$missing" = 0 ] && ok "every delivered runbook link resolves to a file"

# A repeat must refresh, not accumulate. A monitor on a five-minute timer through
# a two-hour outage would otherwise produce 24 items per class.
# OPEN rows only. A recurrence after the alert was resolved is deliberately a new
# item — see platform_alerts.sql — so counting resolved rows too would read a
# correct recurrence as accumulation. This assertion is about a repeat while the
# alert is still open, which is the case that would flood an inbox.
run "${HEALTHY[@]}" -disk-percent 1
before=$("${PSQL_SUPER[@]}" -d "$DB" -tAc "SELECT count(*) FROM attention_items WHERE dedupe_key='monitor:disk' AND status='open'")
run "${HEALTHY[@]}" -disk-percent 1
after=$("${PSQL_SUPER[@]}" -d "$DB" -tAc "SELECT count(*) FROM attention_items WHERE dedupe_key='monitor:disk' AND status='open'")
if [ "$before" = "$after" ]; then ok "a repeated alert refreshed rather than accumulating ($after item(s))"
else bad "a repeat grew the inbox from $before to $after"; fi

# And the alert must go away on its own when the condition does. Otherwise every
# alert needs manual clearing, including the ones that fixed themselves.
run "${HEALTHY[@]}"
open_disk=$("${PSQL_SUPER[@]}" -d "$DB" -tAc \
  "SELECT count(*) FROM attention_items WHERE dedupe_key='monitor:disk' AND status='open'")
if [ "$open_disk" = "0" ]; then ok "the alert cleared itself once the condition passed"
else bad "$open_disk disk alert(s) still open after recovery"; fi

# The recovery must not have closed the alerts whose conditions are still true.
# A monitor that resolves everything on a healthy pass is worse than none.
open_backup=$("${PSQL_SUPER[@]}" -d "$DB" -tAc \
  "SELECT count(*) FROM attention_items WHERE dedupe_key='monitor:webhook' AND status='open'")
if [ "$open_backup" = "$admins" ]; then ok "the still-failing webhook alert stayed open"
else bad "the webhook alert was closed while its delivery is still unprocessed"; fi

echo
echo "----------------------------------------"
printf 'passed %d, failed %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
