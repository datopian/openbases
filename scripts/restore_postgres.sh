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
SRC="$BASE_DIR/$BACKUP"
[ -d "$SRC" ] || { echo "no such backup: $SRC" >&2; exit 1; }

echo "restoring $BACKUP -> $TARGET_DIR (port $PORT)"

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
# --exclude the dump: it is a logical copy stored alongside, not part of the
# physical data directory, and PostgreSQL would refuse to start with a stray file
# it does not recognise in some layouts.
cp -a "$SRC/." "$TARGET_DIR/"
rm -f "$TARGET_DIR"/*.dump

# ---------------------------------------------------------------------------
# Recovery configuration
# ---------------------------------------------------------------------------
#
# archive_mode is turned OFF in the restored copy, and that is not cosmetic. Left
# on, this instance would archive its own WAL into the SAME archive directory the
# live cluster uses, and after promotion its timeline would write segment names
# that collide with the real ones. The archive command refuses to overwrite a
# differing file, so the collision would surface as archiving failures on a
# drill — or, worse, as an archive that no longer describes one history.
{
  echo ""
  echo "# Added by restore_postgres.sh (WP-I2)"
  echo "port = $PORT"
  echo "archive_mode = off"
  echo "restore_command = 'cp $ARCHIVE_DIR/%f %p'"
  echo "unix_socket_directories = '$TARGET_DIR'"
  # A restored instance must never be reachable from anywhere else, whatever the
  # live cluster's settings were.
  echo "listen_addresses = '127.0.0.1'"
  if [ -n "$TARGET_TIME" ]; then
    echo "recovery_target_time = '$TARGET_TIME'"
    # Stop and stay stopped at the target rather than promoting, so the operator
    # can look before committing. promote would end recovery irreversibly.
    echo "recovery_target_action = 'pause'"
  fi
} >> "$TARGET_DIR/postgresql.conf"

# postgresql.auto.conf carries settings written by ALTER SYSTEM on the source
# cluster. Left in place it silently overrides the block above — including the
# port and archive_mode — because auto.conf is read last.
: > "$TARGET_DIR/postgresql.auto.conf"

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
