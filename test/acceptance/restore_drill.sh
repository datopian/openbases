#!/usr/bin/env bash
# WP-I2 acceptance: a timed restore drill that produces evidence.
#
#   sudo -u postgres bash test/acceptance/restore_drill.sh [--evidence FILE]
#
# Three things must be shown, and they are the three the plan asks for:
#
#   data can be restored from a production-shaped backup;
#   the documented RPO (15 minutes) and RTO (4 hours) are met, measured;
#   evidence is produced rather than asserted.
#
# WHAT THIS DOES NOT DO, deliberately: it does not destroy the live cluster. It
# restores into a scratch directory on a separate port and compares the restored
# contents against the original. That proves the backup is restorable and
# measures how long restoring takes, which is the whole of the RTO question,
# without an outage on a shared environment.
#
# Replacing the live data directory is a separate, human-approved act with its
# own runbook (docs/runbooks/restore-postgresql.md). A drill that requires taking
# staging down is a drill that gets run once and then skipped, and the failure
# mode this exists to catch — an unrestorable backup — is caught either way.
set -uo pipefail

# Debian's pg_wrapper symlinks only SOME PostgreSQL binaries into /usr/bin.
# pg_verifybackup and pg_ctl are not among them, so a script that relies on PATH
# alone fails with "pg_verifybackup is not installed" while the binary sits in
# /usr/lib/postgresql/16/bin. Prepending in ascending version order leaves the
# newest first.
#
# Done here rather than only in the systemd unit because the runbooks tell an
# operator to run this by hand during an incident, and it has to work then.
for _pgbin in $(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | sort -V); do
  PATH="$_pgbin:$PATH"
done
export PATH

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BASE_DIR="${WG_BACKUP_BASE_DIR:-/var/lib/workgraph/backups/postgres}"
ARCHIVE_DIR="${WG_WAL_ARCHIVE_DIR:-/var/lib/workgraph/wal-archive}"
EVIDENCE=""
PORT="${WG_DRILL_PORT:-5433}"

# The documented objectives, from plan section 15.1. Written here so the drill
# fails when reality drifts from the promise, rather than reporting a number for
# somebody to compare by eye.
RPO_SECONDS=$(( 15 * 60 ))
RTO_SECONDS=$(( 4 * 60 * 60 ))

while [ "$#" -gt 0 ]; do
  case "$1" in
    --evidence) EVIDENCE="$2"; shift 2 ;;
    --port)     PORT="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

pass=0; fail=0
ok()  { printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass+1)); }
bad() { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail+1)); }

WORK="$(mktemp -d /tmp/wg-drill.XXXXXX)"
chmod 700 "$WORK"
RESTORE_DIR="$WORK/data"
cleanup() {
  pg_ctl --pgdata="$RESTORE_DIR" stop --mode=immediate >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

echo "WP-I2 restore drill"
echo

# ---------------------------------------------------------------------------
# A marker in the live database, so the drill proves it restored THIS data
# ---------------------------------------------------------------------------
#
# Without a marker, a restore of an empty or stale backup can pass a "does it
# start and have tables" check. The marker is written now, and it must come back
# out of the restored copy — which also measures the real recovery point, because
# the marker's age at restore time IS the data loss.
MARKER="drill-$(date -u +%Y%m%dT%H%M%SZ)-$$"
# INSERT only. The table is created by migration 0021 — a test that runs
# CREATE TABLE against the live database is an unreviewed schema change, and it
# would leave the deployed schema and db/migrations disagreeing.
if ! psql -d workgraph -qc \
    "INSERT INTO restore_drill_markers (marker) VALUES ('$MARKER')" >/dev/null 2>&1; then
  echo "cannot write a drill marker to the live database" >&2
  echo "is migration 0021_restore_drill_markers.sql applied?" >&2
  exit 2
fi
MARKER_AT="$(psql -d workgraph -tAc \
  "SELECT written_at FROM restore_drill_markers WHERE marker='$MARKER'")"
echo "  marker $MARKER written at $MARKER_AT"

# ---------------------------------------------------------------------------
# Force the marker into the archive, which is what an RPO actually depends on
# ---------------------------------------------------------------------------
#
# A committed transaction is only recoverable once its WAL is archived. Switching
# the segment here is what the archive_timeout setting does automatically every
# few minutes; doing it explicitly means the drill measures the restore rather
# than waiting for a timer.
psql -d workgraph -qc "SELECT pg_switch_wal()" >/dev/null 2>&1
sleep 2
ARCHIVED_BEFORE="$(find "$ARCHIVE_DIR" -maxdepth 1 -type f 2>/dev/null | wc -l | tr -d ' ')"
echo "  $ARCHIVED_BEFORE segment(s) in the archive"

# ---------------------------------------------------------------------------
# A backup taken NOW, so the drill exercises the real path
# ---------------------------------------------------------------------------
echo
echo "Taking a fresh base backup"
BACKUP_START=$(date +%s)
if ! bash "$ROOT/scripts/backup_postgres.sh" >"$WORK/backup.log" 2>&1; then
  bad "the backup script failed"
  tail -15 "$WORK/backup.log" | sed 's/^/        /'
  echo; echo "passed $pass, failed $fail"; exit 1
fi
BACKUP_SECONDS=$(( $(date +%s) - BACKUP_START ))
ok "backup taken and verified in ${BACKUP_SECONDS}s"
grep -E "verified|logical dump|backup complete" "$WORK/backup.log" | sed 's/^/        /'

# ---------------------------------------------------------------------------
# The restore, timed. This is the RTO measurement.
# ---------------------------------------------------------------------------
echo
echo "Restoring into a scratch instance"
RESTORE_START=$(date +%s)
if ! bash "$ROOT/scripts/restore_postgres.sh" --to "$RESTORE_DIR" --port "$PORT" \
      --base-dir "$BASE_DIR" --archive-dir "$ARCHIVE_DIR" --verify \
      >"$WORK/restore.log" 2>&1; then
  bad "the restore failed"
  tail -20 "$WORK/restore.log" | sed 's/^/        /'
  echo; echo "passed $pass, failed $fail"; exit 1
fi
RESTORE_SECONDS=$(( $(date +%s) - RESTORE_START ))
ok "restored and started in ${RESTORE_SECONDS}s"
grep -E "verified|table\(s\) in the restored" "$WORK/restore.log" | sed 's/^/        /'

# ---------------------------------------------------------------------------
# Did it restore THIS data
# ---------------------------------------------------------------------------
echo
echo "Checking what came back"
# No `tr -d ' '`: psql -tA already returns the value unpadded, and stripping
# spaces removed the one INSIDE the timestamp, turning
# "2026-08-21 16:39:47+00" into "2026-08-2116:39:47+00" — which PostgreSQL then
# rejected as out of range, so the recovery point read as unmeasurable on a
# restore that had in fact worked perfectly.
RESTORED_AT="$(psql -h "$RESTORE_DIR" -p "$PORT" -d workgraph -tAc \
  "SELECT written_at FROM restore_drill_markers WHERE marker='$MARKER'" 2>/dev/null)"
if [ -n "$RESTORED_AT" ]; then
  ok "the drill marker survived the round trip"
else
  bad "the drill marker is NOT in the restored database; the backup predates it or the restore lost it"
fi

# Row counts on the tables that carry the actual work, compared against live.
# "It started and has tables" would pass on a backup of an empty database.
mismatch=0
for t in projects work_refs attention_items audit_log credential_registry; do
  live="$(psql -d workgraph -tAc "SELECT count(*) FROM $t" 2>/dev/null | tr -d ' ')"
  rest="$(psql -h "$RESTORE_DIR" -p "$PORT" -d workgraph -tAc "SELECT count(*) FROM $t" 2>/dev/null | tr -d ' ')"
  if [ "${live:-x}" = "${rest:-y}" ]; then
    printf '        %-22s %s rows, matched\n' "$t" "$live"
  else
    bad "$t differs: live $live, restored $rest"
    mismatch=1
  fi
done
[ "$mismatch" = 0 ] && ok "every checked table matched the live database"

# ---------------------------------------------------------------------------
# The objectives
# ---------------------------------------------------------------------------
echo
echo "Objectives"

# RPO: how much data would have been lost. Measured as the age of the newest
# committed marker that survived, not as the configured archive_timeout — a
# setting is an intention and this is a measurement.
if [ -n "$RESTORED_AT" ]; then
  RPO_ACTUAL="$(psql -d workgraph -tAc \
    "SELECT GREATEST(0, floor(EXTRACT(EPOCH FROM (now() - timestamptz '$RESTORED_AT'))))::bigint")"
  if [ "${RPO_ACTUAL:-999999}" -le "$RPO_SECONDS" ]; then
    ok "recovery point ${RPO_ACTUAL}s, within the documented ${RPO_SECONDS}s"
  else
    bad "recovery point ${RPO_ACTUAL}s exceeds the documented ${RPO_SECONDS}s"
  fi
else
  bad "recovery point cannot be measured: the marker did not survive"
  RPO_ACTUAL="null"
fi

TOTAL_SECONDS=$(( BACKUP_SECONDS + RESTORE_SECONDS ))
if [ "$RESTORE_SECONDS" -le "$RTO_SECONDS" ]; then
  ok "recovery time ${RESTORE_SECONDS}s, within the documented ${RTO_SECONDS}s"
else
  bad "recovery time ${RESTORE_SECONDS}s exceeds the documented ${RTO_SECONDS}s"
fi

# A restore that is fast because the database is tiny says nothing about a
# restore of a large one. Recording the size makes the number interpretable
# later rather than reassuring now.
DB_SIZE="$(psql -d workgraph -tAc "SELECT pg_size_pretty(pg_database_size('workgraph'))" | tr -d ' ')"
ARCHIVE_COUNT="$(find "$ARCHIVE_DIR" -maxdepth 1 -type f 2>/dev/null | wc -l | tr -d ' ')"

echo
echo "----------------------------------------"
printf 'passed %d, failed %d\n' "$pass" "$fail"

if [ -n "$EVIDENCE" ]; then
  cat > "$EVIDENCE" <<JSON
{
  "drill": "postgresql-restore",
  "work_package": "WP-I2",
  "at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "host": "$(hostname)",
  "postgresql_version": "$(psql -tAc 'SHOW server_version' | tr -d ' ')",
  "database_size": "$DB_SIZE",
  "wal_segments_archived": $ARCHIVE_COUNT,
  "marker": "$MARKER",
  "marker_written_at": "$MARKER_AT",
  "marker_restored_at": "${RESTORED_AT:-null}",
  "backup_seconds": $BACKUP_SECONDS,
  "restore_seconds": $RESTORE_SECONDS,
  "total_seconds": $TOTAL_SECONDS,
  "rpo_seconds_observed": ${RPO_ACTUAL:-null},
  "rpo_seconds_documented": $RPO_SECONDS,
  "rto_seconds_documented": $RTO_SECONDS,
  "destructive": false,
  "note": "Restored into a scratch instance on port $PORT and compared against the live database. The live cluster was not modified beyond writing one marker row.",
  "checks_passed": $pass,
  "checks_failed": $fail
}
JSON
  echo "evidence written to $EVIDENCE"
fi

[ "$fail" -eq 0 ] || exit 1
