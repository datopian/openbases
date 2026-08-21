#!/usr/bin/env bash
# Restore PostgreSQL from a base backup, optionally to a point in time (WP-I2).
#
#   restore_postgres.sh --to DIR [--backup NAME|latest] [--target-time 'ISO8601']
#                       [--port N] [--verify]
#
# Run as postgres. This restores into a DIRECTORY YOU NAME and starts a SEPARATE
# instance on its own port. It does not touch the live cluster, because the thing
# you want during an incident is to confirm a backup is good BEFORE replacing
# anything with it — and the thing you want during a drill is to prove
# restorability without an outage.
#
# Replacing the live data directory is a deliberate, separate act, documented in
# docs/runbooks/restore-postgresql.md. It is not a flag here.
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

BASE_DIR="${WG_BACKUP_BASE_DIR:-/var/lib/workgraph/backups/postgres}"
ARCHIVE_DIR="${WG_WAL_ARCHIVE_DIR:-/var/lib/workgraph/wal-archive}"
BACKUP="latest"
TARGET_DIR=""
TARGET_TIME=""
PORT="5433"
VERIFY=0

while [ "$#" -gt 0 ]; do
  case "$1" in
    --to)          TARGET_DIR="$2"; shift 2 ;;
    --backup)      BACKUP="$2"; shift 2 ;;
    --target-time) TARGET_TIME="$2"; shift 2 ;;
    --port)        PORT="$2"; shift 2 ;;
    --base-dir)    BASE_DIR="$2"; shift 2 ;;
    --archive-dir) ARCHIVE_DIR="$2"; shift 2 ;;
    --verify)      VERIFY=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ -n "$TARGET_DIR" ] || { echo "--to DIR is required" >&2; exit 2; }

# Refusing to restore over a live data directory is worth a check rather than a
# warning in a comment. Restoring onto a running cluster's files corrupts it.
if [ -f "$TARGET_DIR/postmaster.pid" ]; then
  echo "$TARGET_DIR has a postmaster.pid: something is running there. Refusing." >&2
  exit 1
fi
if [ -e "$TARGET_DIR" ] && [ -n "$(ls -A "$TARGET_DIR" 2>/dev/null)" ]; then
  echo "$TARGET_DIR is not empty. Refusing to restore into it." >&2
  exit 1
fi

if [ "$BACKUP" = "latest" ]; then
  BACKUP="$(find "$BASE_DIR" -maxdepth 1 -mindepth 1 -type d -printf '%f\n' | sort | tail -1)"
  [ -n "$BACKUP" ] || { echo "no backup found in $BASE_DIR" >&2; exit 1; }
fi
# The physical copy lives in base/, with the logical dump alongside in dump/.
# Only the physical copy is a data directory.
SRC="$BASE_DIR/$BACKUP/base"
[ -d "$SRC" ] || { echo "no such backup: $SRC" >&2; exit 1; }

echo "restoring $BACKUP -> $TARGET_DIR (port $PORT)"

# A recovery target later than the newest archived segment cannot be reached, and
# PostgreSQL does not degrade gracefully: it replays everything it has and then
# exits with "recovery ended before configured recovery target was reached",
# leaving an instance that will not start. Asking for "now" does this, which is
# the obvious thing to type.
#
# The archive lags the present by up to archive_timeout, so the newest recoverable
# moment is always a few minutes ago. Warning up front is cheaper than the
# operator reading a FATAL and concluding the backups are broken.
if [ -n "$TARGET_TIME" ]; then
  newest_seg="$(find "$ARCHIVE_DIR" -maxdepth 1 -type f -name '0*' -printf '%T@ %p\n' 2>/dev/null \
                | sort -n | tail -1 | cut -d' ' -f2-)"
  if [ -n "$newest_seg" ]; then
    newest_at="$(date -u -r "$newest_seg" '+%Y-%m-%d %H:%M:%S+00' 2>/dev/null)"
    echo "  newest archived segment is from $newest_at"
    target_epoch="$(date -u -d "$TARGET_TIME" +%s 2>/dev/null || echo 0)"
    newest_epoch="$(date -u -r "$newest_seg" +%s 2>/dev/null || echo 0)"
    if [ "$target_epoch" -gt "$newest_epoch" ] 2>/dev/null; then
      echo "  WARNING: the requested target is LATER than anything in the archive." >&2
      echo "  Recovery will replay everything available and then fail with" >&2
      echo "  \"recovery ended before configured recovery target was reached\"." >&2
      echo "  Choose a target at or before $newest_at, or omit --target-time to" >&2
      echo "  recover as far as the archive allows." >&2
    fi
  fi
fi

# Verify before trusting. A restore is the worst moment to discover the backup
# was never readable, and it costs a second on a small database.
if [ -f "$SRC/backup_manifest" ]; then
  if ! pg_verifybackup "$SRC" >/dev/null 2>&1; then
    echo "  the backup FAILS verification against its manifest; refusing to restore it" >&2
    exit 1
  fi
  echo "  backup verified against its manifest"
fi

mkdir -p "$TARGET_DIR"
chmod 700 "$TARGET_DIR"
cp -a "$SRC/." "$TARGET_DIR/"

# ---------------------------------------------------------------------------
# Recovery configuration, written from scratch rather than inherited
# ---------------------------------------------------------------------------
#
# On Debian and Ubuntu the configuration files live in /etc/postgresql/NN/main
# and are NOT inside the data directory, so pg_basebackup does not copy them. A
# restored directory therefore has no postgresql.conf and no pg_hba.conf, and the
# server refuses to start: "could not load pg_hba.conf".
#
# Copying the live configuration in would be worse than useless. It sets
# data_directory, hba_file and ident_file to absolute paths pointing at the
# RUNNING cluster, so a scratch instance would read the live cluster's files and,
# depending on what else it inherited, could start against the live data
# directory. That is the one outcome a restore drill must never risk.
#
# So the scratch instance gets its own minimal, self-contained configuration.
# PostgreSQL looks for pg_hba.conf and pg_ident.conf in the data directory when
# nothing says otherwise, which is exactly what is wanted here.
cat > "$TARGET_DIR/postgresql.conf" <<CONF
# Generated by restore_postgres.sh (WP-I2) for a SCRATCH instance.
# Deliberately minimal: nothing here is inherited from the live cluster.
port = $PORT
listen_addresses = '127.0.0.1'
unix_socket_directories = '$TARGET_DIR'

# Archiving OFF, and this is not cosmetic. Left on, this instance would archive
# its own WAL into the SAME directory the live cluster uses, and after promotion
# its timeline would produce segment names that collide with the real ones.
archive_mode = off

# How recovery fetches the segments it needs from the archive. The helper
# decompresses, and still handles the plain segments archived before compression
# was introduced — a restore has to span that boundary, and the boundary is in
# the past, which is where restores happen.
#
# The archive directory is passed EXPLICITLY. --archive-dir was accepted by this
# script from the start and then ignored here, because the helper used the path
# baked into it at deployment time. So a restore always read the local archive
# whatever it was told, which went unnoticed while the only caller was a drill
# that wanted the local archive anyway — and would have quietly defeated a
# restore from an off-machine copy.
restore_command = '/usr/local/bin/wg-restore-wal %f %p $ARCHIVE_DIR'

# Modest, because this runs alongside the live cluster on a small node and must
# not compete with it for memory. shared_buffers is not one of the settings
# recovery constrains, so it is safe to shrink.
shared_buffers = 128MB
CONF

# The settings recovery REFUSES to start below, taken from the backup itself.
#
# PostgreSQL aborts recovery when max_connections, max_worker_processes,
# max_wal_senders, max_prepared_transactions or max_locks_per_transaction is
# lower than it was on the source server, because the WAL it is replaying may
# reference more of those resources than this instance could hold. The values
# were recorded when the backup was taken, so they are available even when the
# source cluster is gone — which is the case that matters.
if [ -f "$BASE_DIR/$BACKUP/restore_settings.conf" ]; then
  cat "$BASE_DIR/$BACKUP/restore_settings.conf" >> "$TARGET_DIR/postgresql.conf"
  echo "  applied $(wc -l < "$BASE_DIR/$BACKUP/restore_settings.conf" | tr -d ' ') recovery setting(s) recorded with the backup"
else
  # An older backup, taken before these were recorded. The built-in defaults are
  # usually enough; say so rather than failing, and name the symptom so the log
  # line above is recognisable if it is not.
  echo "  no recorded settings with this backup; using defaults." >&2
  echo "  if recovery aborts with 'insufficient parameter settings', raise the" >&2
  echo "  named setting in $TARGET_DIR/postgresql.conf and start it by hand." >&2
fi

if [ -n "$TARGET_TIME" ]; then
  {
    echo "recovery_target_time = '$TARGET_TIME'"
    # Stop and stay stopped at the target rather than promoting, so an operator
    # can look before committing. promote ends recovery irreversibly.
    echo "recovery_target_action = 'pause'"
  } >> "$TARGET_DIR/postgresql.conf"
fi

# Local socket only, and trust — the instance listens on its own socket
# directory inside a private, mode-700 directory and on loopback with no
# password role that could be reached. Nothing else can connect.
cat > "$TARGET_DIR/pg_hba.conf" <<'HBA'
# Generated by restore_postgres.sh (WP-I2) for a SCRATCH instance.
local   all   all               trust
host    all   all   127.0.0.1/32  trust
HBA
: > "$TARGET_DIR/pg_ident.conf"

# postgresql.auto.conf carries settings written by ALTER SYSTEM on the source
# cluster and is read LAST, so anything in it silently overrides the file above —
# including the port and archive_mode.
: > "$TARGET_DIR/postgresql.auto.conf"

chmod 600 "$TARGET_DIR/postgresql.conf" "$TARGET_DIR/pg_hba.conf" \
          "$TARGET_DIR/pg_ident.conf" "$TARGET_DIR/postgresql.auto.conf"

# The signal file is what makes this a recovery rather than a normal start.
touch "$TARGET_DIR/recovery.signal"

echo "  starting the restored instance"
LOG="$TARGET_DIR/restore.log"
if ! pg_ctl --pgdata="$TARGET_DIR" --log="$LOG" --wait --timeout=300 start >/dev/null 2>&1; then
  echo "  the restored instance did not start; last lines of $LOG:" >&2
  tail -20 "$LOG" >&2
  exit 1
fi

# Recovery may still be replaying. Wait for it to finish, or to pause at the
# recovery target if one was given.
for _ in $(seq 1 300); do
  state="$(psql -h "$TARGET_DIR" -p "$PORT" -d postgres -tAc \
    "SELECT CASE WHEN pg_is_in_recovery() THEN 'recovering' ELSE 'ready' END" 2>/dev/null)"
  [ "$state" = "ready" ] && break
  if [ -n "$TARGET_TIME" ] && grep -q "recovery stopping before\|pausing at the end of recovery" "$LOG" 2>/dev/null; then
    state="paused"; break
  fi
  sleep 1
done

echo "  instance state: ${state:-unknown}"
echo "  connect with: psql -h $TARGET_DIR -p $PORT -d postgres"
echo "  stop with:    pg_ctl -D $TARGET_DIR stop"

if [ "$VERIFY" = 1 ]; then
  echo "  verifying the restored database answers"
  if [ "${state:-}" = "paused" ]; then
    # A paused recovery does not accept queries on all versions; promote so the
    # data can be inspected. This is a scratch instance, so promoting it costs
    # nothing.
    psql -h "$TARGET_DIR" -p "$PORT" -d postgres -c "SELECT pg_wal_replay_resume()" >/dev/null 2>&1
    pg_ctl --pgdata="$TARGET_DIR" promote >/dev/null 2>&1
    sleep 3
  fi
  if ! psql -h "$TARGET_DIR" -p "$PORT" -d workgraph -tAc \
      "SELECT count(*) FROM pg_tables WHERE schemaname='public'" 2>/dev/null \
      | { read -r n; echo "    $n table(s) in the restored database"; [ "${n:-0}" -ge 35 ]; }; then
    echo "  the restored database does not contain the expected schema" >&2
    exit 1
  fi
fi
