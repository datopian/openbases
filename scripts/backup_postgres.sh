#!/usr/bin/env bash
# Take a verified PostgreSQL base backup (WP-I2).
#
#   backup_postgres.sh [--base-dir DIR] [--keep N] [--receipt-dir DIR]
#
# Run as postgres on the control node, from a systemd timer. Together with WAL
# archiving this is what makes the 15-minute recovery point objective reachable:
# a base backup alone would put the RPO at however long ago it ran, which for a
# daily backup is up to 24 hours.
#
# pg_basebackup rather than pg_dump, and both rather than either:
#
#   pg_basebackup is a physical copy, so it can be combined with archived WAL to
#   recover to any point in time. That is the only way to hit a 15-minute RPO.
#
#   pg_dump is logical, so it survives the class of failure a physical copy
#   faithfully reproduces — a corrupted page, or a bad migration. It is also the
#   only form that can be restored into a different PostgreSQL major version,
#   which matters during an upgrade.
#
# The dump is taken here as well because the two together cost seconds on a
# 25 MB database, and the day one is needed is not the day to discover the other
# was the only one being taken.
set -uo pipefail

BASE_DIR="${WG_BACKUP_BASE_DIR:-/var/lib/workgraph/backups/postgres}"
RECEIPT_DIR="${WG_BACKUP_RECEIPT_DIR:-/var/lib/workgraph/backups}"
KEEP="${WG_BACKUP_KEEP:-30}"
DATABASE="${WG_BACKUP_DATABASE:-workgraph}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --base-dir)    BASE_DIR="$2"; shift 2 ;;
    --keep)        KEEP="$2"; shift 2 ;;
    --receipt-dir) RECEIPT_DIR="$2"; shift 2 ;;
    --database)    DATABASE="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

command -v pg_basebackup   >/dev/null || { echo "pg_basebackup is not installed" >&2; exit 2; }
command -v pg_verifybackup >/dev/null || { echo "pg_verifybackup is not installed" >&2; exit 2; }

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
TARGET="$BASE_DIR/$STAMP"
mkdir -p "$BASE_DIR" || { echo "cannot create $BASE_DIR" >&2; exit 1; }

echo "postgres backup $STAMP"

# A backup that half-completed must not be left where a restore might find it
# and trust it. Anything that fails below is removed, and the receipt — which is
# what the monitor reads — is only written at the very end.
cleanup_failed() {
  if [ -d "$TARGET" ]; then
    echo "  removing the incomplete backup at $TARGET" >&2
    rm -rf "$TARGET"
  fi
}

# -Fp (plain) rather than tar, because pg_verifybackup can check a plain backup
# in place. Verifying a tar archive means extracting it first, and a verification
# step that is awkward to run is one that quietly stops being run.
#
# -X stream copies the WAL generated DURING the backup, so this directory is
# restorable on its own even if the archive is unreachable. It is belt and
# braces with the archive, and it is what makes the backup independently useful.
#
# --checkpoint=fast so the backup starts now instead of waiting for the next
# scheduled checkpoint, which on an idle database can be minutes away.
if ! pg_basebackup \
      --pgdata="$TARGET" \
      --format=plain \
      --wal-method=stream \
      --checkpoint=fast \
      --progress \
      --no-password 2>&1 | sed 's/^/  /'; then
  echo "  pg_basebackup failed" >&2
  cleanup_failed
  exit 1
fi

# Automated verification of backup readability, which the plan requires and
# which is the difference between having a backup and believing you have one.
# pg_verifybackup checks every file against the manifest's checksums.
if ! pg_verifybackup "$TARGET" 2>&1 | sed 's/^/  /'; then
  echo "  pg_verifybackup REJECTED the backup; it is not restorable" >&2
  cleanup_failed
  exit 1
fi
echo "  verified against the backup manifest"

# The logical dump, next to the physical one so they are pruned together.
if ! pg_dump --format=custom --compress=9 --file="$TARGET/$DATABASE.dump" "$DATABASE"; then
  echo "  pg_dump failed" >&2
  cleanup_failed
  exit 1
fi

# A dump is not verified by being produced. pg_restore --list parses the archive
# and fails on a truncated or corrupt file, which is the cheap check that the
# bytes are readable.
if ! pg_restore --list "$TARGET/$DATABASE.dump" >/dev/null; then
  echo "  the dump was written but is not readable by pg_restore" >&2
  cleanup_failed
  exit 1
fi
echo "  logical dump written and readable ($(du -h "$TARGET/$DATABASE.dump" | cut -f1))"

SIZE="$(du -sh "$TARGET" | cut -f1)"
echo "  backup complete: $TARGET ($SIZE)"

# Retention. Prune AFTER a successful backup, never before: pruning first would
# mean a failing backup slowly deletes the history that is all you have left.
mapfile -t all < <(find "$BASE_DIR" -maxdepth 1 -mindepth 1 -type d -printf '%f\n' | sort)
if [ "${#all[@]}" -gt "$KEEP" ]; then
  drop=$(( ${#all[@]} - KEEP ))
  echo "  pruning $drop backup(s) beyond the $KEEP most recent"
  for d in "${all[@]:0:$drop}"; do
    rm -rf "$BASE_DIR/$d" && echo "    removed $d"
  done
fi

# The receipt, last, and only now.
#
# The monitor reads the timestamp INSIDE this file rather than its mtime, so a
# copy or a stray touch cannot pass for a backup. Written after verification, so
# its meaning is "a backup existed and was checked", not "a command exited 0".
if [ -d "$RECEIPT_DIR" ] && [ -w "$RECEIPT_DIR" ]; then
  date -u +%Y-%m-%dT%H:%M:%SZ > "$RECEIPT_DIR/postgres.receipt"
  echo "  receipt written to $RECEIPT_DIR/postgres.receipt"
else
  echo "  no writable receipt directory at $RECEIPT_DIR; the monitor will keep" >&2
  echo "  reporting this backup as missing even though it succeeded" >&2
  exit 1
fi
