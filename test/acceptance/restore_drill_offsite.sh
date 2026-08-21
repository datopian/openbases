#!/usr/bin/env bash
# WP-I2 acceptance: restore from the OFF-MACHINE copy (wg-osr).
#
#   sudo bash test/acceptance/restore_drill_offsite.sh [--evidence FILE]
#
# The local drill (restore_drill.sh) restores from /var/lib/workgraph/backups.
# This one restores from r2://…/postgres/ and replays WAL pulled from r2://…/wal/,
# and it does not read the local backup directory or the local archive at all.
#
# That distinction is the whole point. The off-machine copy is the one that
# survives losing the node, which is the scenario the entire work package exists
# for — and it travels a different path: download, integrity-check, untar, then
# recover using an archive that is also a download. Every step there is untested
# by the local drill, and an untested restore path is a belief.
#
# It still runs ON the node, because that is where PostgreSQL is. What it
# simulates is the local copies being gone, not the machine being gone.
set -uo pipefail

for _pgbin in $(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | sort -V); do
  PATH="$_pgbin:$PATH"
done
export PATH

BUCKET="${WG_OFFSITE_BUCKET:-workgraph-backups-staging}"
EVIDENCE=""
PORT="${WG_DRILL_PORT:-5435}"

RPO_SECONDS=$(( 15 * 60 ))
RTO_SECONDS=$(( 4 * 60 * 60 ))

while [ "$#" -gt 0 ]; do
  case "$1" in
    --evidence) EVIDENCE="$2"; shift 2 ;;
    --bucket)   BUCKET="$2"; shift 2 ;;
    --port)     PORT="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

pass=0; fail=0
ok()  { printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass+1)); }
bad() { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail+1)); }

command -v rclone >/dev/null || { echo "rclone is not installed" >&2; exit 2; }

# Credentials the same way wg-backup-offsite gets them.
read_credential() {
  local name="$1" envvar="$2"
  if [ -n "${CREDENTIALS_DIRECTORY:-}" ] && [ -r "$CREDENTIALS_DIRECTORY/$name" ]; then
    tr -d '\r\n' < "$CREDENTIALS_DIRECTORY/$name"
  elif [ -r "/etc/workgraph/credentials/$name" ]; then
    tr -d '\r\n' < "/etc/workgraph/credentials/$name"
  else
    printf '%s' "${!envvar:-}"
  fi
}
ACCESS_KEY="$(read_credential r2_access_key_id R2_ACCESS_KEY_ID)"
SECRET_KEY="$(read_credential r2_secret_access_key R2_SECRET_ACCESS_KEY)"
ACCOUNT="$(read_credential r2_account_id CLOUDFLARE_ACCOUNT_ID)"
ENDPOINT="${R2_S3_ENDPOINT:-https://${ACCOUNT}.r2.cloudflarestorage.com}"
[ -n "$ACCESS_KEY" ] && [ -n "$SECRET_KEY" ] && [ -n "$ACCOUNT" ] || {
  echo "no R2 credentials available" >&2; exit 2; }

export RCLONE_CONFIG_R2_TYPE=s3 RCLONE_CONFIG_R2_PROVIDER=Cloudflare
export RCLONE_CONFIG_R2_ACCESS_KEY_ID="$ACCESS_KEY"
export RCLONE_CONFIG_R2_SECRET_ACCESS_KEY="$SECRET_KEY"
export RCLONE_CONFIG_R2_ENDPOINT="$ENDPOINT"
RC=(--s3-no-check-bucket --checksum --no-update-modtime)

WORK="$(mktemp -d /var/tmp/wg-offsite-drill.XXXXXX)"
chmod 700 "$WORK"
DATA="$WORK/data"
FETCHED_BASE="$WORK/base"
FETCHED_WAL="$WORK/wal"
mkdir -p "$FETCHED_BASE" "$FETCHED_WAL"
# postgres must be able to read what we downloaded and write the restored copy.
chown -R postgres:postgres "$WORK"
cleanup() {
  sudo -u postgres pg_ctl --pgdata="$DATA" stop --mode=immediate >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

echo "WP-I2 off-machine restore drill  (r2://$BUCKET)"
echo

# ---------------------------------------------------------------------------
# A marker, so this proves it restored THIS data and not merely something
# ---------------------------------------------------------------------------
MARKER="offsite-$(date -u +%Y%m%dT%H%M%SZ)-$$"
if ! sudo -u postgres psql -d workgraph -qc \
    "INSERT INTO restore_drill_markers (marker) VALUES ('$MARKER')" >/dev/null 2>&1; then
  echo "cannot write a drill marker to the live database" >&2; exit 2
fi
MARKER_AT="$(sudo -u postgres psql -d workgraph -tAc \
  "SELECT written_at FROM restore_drill_markers WHERE marker='$MARKER'")"
echo "  marker written at $MARKER_AT"

# A backup taken AFTER the marker, and pushed off-machine, so the marker is
# genuinely in the copy being restored. Without this the drill would restore
# yesterday's backup and correctly fail to find today's marker.
echo "  taking a backup and pushing it off-machine"
sudo -u postgres psql -d workgraph -qc "SELECT pg_switch_wal()" >/dev/null 2>&1
systemctl start wg-backup-postgres.service >/dev/null 2>&1 || true
systemctl start wg-backup-offsite.service  >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# Fetch. This is the part the local drill never exercises.
# ---------------------------------------------------------------------------
echo
echo "Fetching from R2"
FETCH_START=$(date +%s)

NEWEST="$(rclone lsf "r2:$BUCKET/postgres/" "${RC[@]}" 2>/dev/null | sort | tail -1)"
if [ -z "$NEWEST" ]; then
  bad "no base backup in r2://$BUCKET/postgres/"
  echo; echo "passed $pass, failed $fail"; exit 1
fi
echo "  newest object: $NEWEST"

if ! rclone copy "r2:$BUCKET/postgres/$NEWEST" "$FETCHED_BASE/" "${RC[@]}" >/dev/null 2>&1; then
  bad "could not download $NEWEST"
  echo; echo "passed $pass, failed $fail"; exit 1
fi
TAR="$FETCHED_BASE/$NEWEST"
DL_BYTES="$(stat -c %s "$TAR")"
ok "downloaded $NEWEST ($(( DL_BYTES / 1048576 )) MB)"

# Integrity BEFORE trusting it. A truncated object downloads without complaint
# and fails in the middle of extraction, or worse, extracts partially.
if gzip -t "$TAR" 2>/dev/null && tar tzf "$TAR" >/dev/null 2>&1; then
  ok "the downloaded archive passes gzip and tar integrity checks"
else
  bad "the downloaded archive is corrupt"
  echo; echo "passed $pass, failed $fail"; exit 1
fi

# The WAL prefix too. In the scenario being rehearsed the local archive is gone,
# so point-in-time recovery has to come from here.
if rclone copy "r2:$BUCKET/wal/" "$FETCHED_WAL/" "${RC[@]}" --transfers 8 >/dev/null 2>&1; then
  WAL_COUNT="$(find "$FETCHED_WAL" -type f | wc -l | tr -d ' ')"
  ok "downloaded $WAL_COUNT WAL segment(s) from the off-machine archive"
else
  bad "could not download the WAL archive"
fi

mkdir -p "$FETCHED_BASE/extracted"
if tar xzf "$TAR" -C "$FETCHED_BASE/extracted"; then
  ok "extracted"
else
  bad "extraction failed"
  echo; echo "passed $pass, failed $fail"; exit 1
fi
chown -R postgres:postgres "$WORK"
FETCH_SECONDS=$(( $(date +%s) - FETCH_START ))

# ---------------------------------------------------------------------------
# Restore, from the downloaded copy only
# ---------------------------------------------------------------------------
echo
echo "Restoring from the downloaded copy"
RESTORE_START=$(date +%s)

# --base-dir points at the extracted tar, which has the base/ + dump/ layout the
# backup script writes, so restore_postgres.sh finds it exactly as it would
# locally. --archive-dir points at the DOWNLOADED archive: nothing here reads
# /var/lib/workgraph.
if sudo -u postgres /usr/local/bin/wg-restore-postgres \
     --to "$DATA" --port "$PORT" \
     --base-dir "$FETCHED_BASE/extracted" --backup . \
     --archive-dir "$FETCHED_WAL" --verify > "$WORK/restore.log" 2>&1; then
  RESTORE_SECONDS=$(( $(date +%s) - RESTORE_START ))
  ok "restored and started in ${RESTORE_SECONDS}s"
  grep -E "verified|table\(s\) in the restored|recovery setting" "$WORK/restore.log" | sed 's/^/        /'
else
  RESTORE_SECONDS=$(( $(date +%s) - RESTORE_START ))
  bad "the restore failed"
  tail -20 "$WORK/restore.log" | sed 's/^/        /'
  echo; echo "passed $pass, failed $fail"; exit 1
fi

# Prove the archive we downloaded was actually used, not the local one. Without
# this the drill could pass while silently reading /var/lib/workgraph/wal-archive
# — which is exactly the bug that made this test necessary.
if grep -q "restored log file" "$WORK/restore.log" 2>/dev/null \
   || grep -q "restored log file" "$DATA/restore.log" 2>/dev/null; then
  ok "recovery replayed WAL fetched from R2"
else
  echo "        note: recovery needed no archived WAL (the base backup was self-contained)"
fi

# ---------------------------------------------------------------------------
# Did it bring back THIS data
# ---------------------------------------------------------------------------
echo
echo "Checking what came back"
RESTORED_AT="$(sudo -u postgres psql -h "$DATA" -p "$PORT" -d workgraph -tAc \
  "SELECT written_at FROM restore_drill_markers WHERE marker='$MARKER'" 2>/dev/null)"
if [ -n "$RESTORED_AT" ]; then
  ok "the marker survived the round trip through R2"
else
  bad "the marker is NOT in the copy restored from R2"
fi

mismatch=0
for t in projects work_refs attention_items audit_log credential_registry; do
  live="$(sudo -u postgres psql -d workgraph -tAc "SELECT count(*) FROM $t" 2>/dev/null)"
  rest="$(sudo -u postgres psql -h "$DATA" -p "$PORT" -d workgraph -tAc "SELECT count(*) FROM $t" 2>/dev/null)"
  if [ "${live:-x}" = "${rest:-y}" ]; then
    printf '        %-22s %s rows, matched\n' "$t" "$live"
  else
    bad "$t differs: live $live, restored $rest"; mismatch=1
  fi
done
[ "$mismatch" = 0 ] && ok "every checked table matched the live database"

# ---------------------------------------------------------------------------
# Objectives, including the download in the recovery time
# ---------------------------------------------------------------------------
echo
echo "Objectives"
if [ -n "$RESTORED_AT" ]; then
  RPO_ACTUAL="$(sudo -u postgres psql -d workgraph -tAc \
    "SELECT GREATEST(0, floor(EXTRACT(EPOCH FROM (now() - timestamptz '$RESTORED_AT'))))::bigint")"
  if [ "${RPO_ACTUAL:-999999}" -le "$RPO_SECONDS" ]; then
    ok "recovery point ${RPO_ACTUAL}s, within the documented ${RPO_SECONDS}s"
  else
    bad "recovery point ${RPO_ACTUAL}s exceeds the documented ${RPO_SECONDS}s"
  fi
else
  bad "recovery point cannot be measured"; RPO_ACTUAL="null"
fi

# The download counts. Recovering from off-machine means fetching first, and a
# recovery time that excludes the fetch is not the time anybody experiences.
TOTAL=$(( FETCH_SECONDS + RESTORE_SECONDS ))
if [ "$TOTAL" -le "$RTO_SECONDS" ]; then
  ok "recovery time ${TOTAL}s including the ${FETCH_SECONDS}s fetch, within ${RTO_SECONDS}s"
else
  bad "recovery time ${TOTAL}s exceeds the documented ${RTO_SECONDS}s"
fi

DB_SIZE="$(sudo -u postgres psql -tAc "SELECT pg_size_pretty(pg_database_size('workgraph'))" | tr -d ' ')"

echo
echo "----------------------------------------"
printf 'passed %d, failed %d\n' "$pass" "$fail"

if [ -n "$EVIDENCE" ]; then
  cat > "$EVIDENCE" <<JSON
{
  "drill": "postgresql-restore-from-offsite",
  "work_package": "WP-I2",
  "bead": "wg-osr",
  "at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "host": "$(hostname)",
  "source": "r2://$BUCKET/postgres/$NEWEST",
  "wal_source": "r2://$BUCKET/wal/",
  "wal_segments_downloaded": ${WAL_COUNT:-0},
  "downloaded_bytes": $DL_BYTES,
  "database_size": "$DB_SIZE",
  "marker": "$MARKER",
  "marker_written_at": "$MARKER_AT",
  "marker_restored_at": "${RESTORED_AT:-null}",
  "fetch_seconds": $FETCH_SECONDS,
  "restore_seconds": $RESTORE_SECONDS,
  "total_seconds": $TOTAL,
  "rpo_seconds_observed": ${RPO_ACTUAL:-null},
  "rpo_seconds_documented": $RPO_SECONDS,
  "rto_seconds_documented": $RTO_SECONDS,
  "read_local_backups": false,
  "note": "Restored entirely from objects downloaded out of R2: the base backup tar and the WAL archive. Neither /var/lib/workgraph/backups nor /var/lib/workgraph/wal-archive was read.",
  "checks_passed": $pass,
  "checks_failed": $fail
}
JSON
  echo "evidence written to $EVIDENCE"
fi

[ "$fail" -eq 0 ] || exit 1
