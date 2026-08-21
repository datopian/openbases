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
RECEIPT_DIR="${WG_BACKUP_RECEIPT_DIR:-/var/lib/workgraph/backups}"
ARCHIVE_DIR="${WG_WAL_ARCHIVE_DIR:-/var/lib/workgraph/wal-archive}"
KEEP="${WG_BACKUP_KEEP:-30}"
DATABASE="${WG_BACKUP_DATABASE:-workgraph}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --base-dir)    BASE_DIR="$2"; shift 2 ;;
    --keep)        KEEP="$2"; shift 2 ;;
    --receipt-dir) RECEIPT_DIR="$2"; shift 2 ;;
    --archive-dir) ARCHIVE_DIR="$2"; shift 2 ;;
    --database)    DATABASE="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

command -v pg_basebackup   >/dev/null || { echo "pg_basebackup is not installed" >&2; exit 2; }
command -v pg_verifybackup >/dev/null || { echo "pg_verifybackup is not installed" >&2; exit 2; }

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
# One directory per backup, holding the physical copy and the logical dump in
# SEPARATE subdirectories.
#
# The dump used to be written inside the pg_basebackup target, and that quietly
# broke re-verification: pg_verifybackup compares the directory against the
# manifest and reports any file the manifest does not list. Verification passed
# at backup time, because the dump was written afterwards, and failed every time
# afterwards — so the backup looked good when taken and refused to restore. The
# restore drill caught it; nothing else would have until a real incident.
RUN="$BASE_DIR/$STAMP"
TARGET="$RUN/base"
DUMP_DIR="$RUN/dump"
mkdir -p "$BASE_DIR" || { echo "cannot create $BASE_DIR" >&2; exit 1; }

echo "postgres backup $STAMP"

# A backup that half-completed must not be left where a restore might find it
# and trust it. Anything that fails below is removed, and the receipt — which is
# what the monitor reads — is only written at the very end.
cleanup_failed() {
  if [ -d "$RUN" ]; then
    echo "  removing the incomplete backup at $RUN" >&2
    rm -rf "$RUN"
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
mkdir -p "$DUMP_DIR"
if ! pg_dump --format=custom --compress=9 --file="$DUMP_DIR/$DATABASE.dump" "$DATABASE"; then
  echo "  pg_dump failed" >&2
  cleanup_failed
  exit 1
fi

# A dump is not verified by being produced. pg_restore --list parses the archive
# and fails on a truncated or corrupt file, which is the cheap check that the
# bytes are readable.
if ! pg_restore --list "$DUMP_DIR/$DATABASE.dump" >/dev/null; then
  echo "  the dump was written but is not readable by pg_restore" >&2
  cleanup_failed
  exit 1
fi
echo "  logical dump written and readable ($(du -h "$DUMP_DIR/$DATABASE.dump" | cut -f1))"

# Verify once more, now that everything has been written, so the state recorded
# by the receipt is the state a restore will actually find.
if ! pg_verifybackup "$TARGET" >/dev/null 2>&1; then
  echo "  the backup no longer verifies after the dump was written" >&2
  cleanup_failed
  exit 1
fi

# Record the settings a RESTORE of this backup will need.
#
# PostgreSQL refuses to finish recovery if certain settings are lower than they
# were on the server the backup came from: "recovery aborted because of
# insufficient parameter settings ... max_connections = 20 is a lower setting
# than on the primary server, where its value was 100." The values live in the
# control file, so recovery knows them and the operator does not.
#
# Captured here so the BACKUP carries what its own restore requires. Querying the
# live cluster at restore time would work on a good day and fail on the only day
# it matters, because during a real disaster the cluster whose settings you need
# is the one that is gone.
#
# Written outside base/ so it is not an extra file the backup manifest does not
# know about — the mistake that made every restore refuse to verify.
if ! psql -tAc "
    SELECT name || ' = ' || setting
      FROM pg_settings
     WHERE name IN ('max_connections', 'max_worker_processes', 'max_wal_senders',
                    'max_prepared_transactions', 'max_locks_per_transaction')
     ORDER BY name" > "$RUN/restore_settings.conf"; then
  echo "  could not record the settings needed to restore this backup" >&2
  cleanup_failed
  exit 1
fi
echo "  recorded $(wc -l < "$RUN/restore_settings.conf" | tr -d ' ') recovery setting(s)"

SIZE="$(du -sh "$RUN" | cut -f1)"
echo "  backup complete: $RUN ($SIZE)"

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

# ---------------------------------------------------------------------------
# WAL archive retention
# ---------------------------------------------------------------------------
#
# This was missing entirely, and it is the more dangerous of the two retentions.
# Base backups are pruned to a count; the WAL archive grew without bound. With
# archive_timeout forcing a 16 MB segment every five minutes, the archive was
# measured growing at 4.6 GB/day — the disk would have filled in about a month
# and the only thing that would have noticed is the disk-pressure alert.
#
# The cutoff is the oldest base backup we still keep. Segments before it can
# never be needed: recovery always starts from a base backup, so WAL preceding
# the earliest one we have is unreachable by definition. Deleting anything NEWER
# would silently break point-in-time recovery, which is why the cutoff is derived
# from the backups on disk rather than from a number of days.
oldest="$(find "$BASE_DIR" -maxdepth 1 -mindepth 1 -type d -printf '%f\n' | sort | head -1)"
label="$BASE_DIR/$oldest/base/backup_label"
if [ -n "$oldest" ] && [ -f "$label" ]; then
  # backup_label records where recovery from that backup must begin:
  #   START WAL LOCATION: 0/1C000028 (file 00000001000000000000001C)
  cutoff="$(sed -n 's/^START WAL LOCATION: .*(file \([0-9A-F]\{24\}\))$/\1/p' "$label" | head -1)"
  if [ -n "$cutoff" ]; then
    before="$(find "$ARCHIVE_DIR" -maxdepth 1 -type f 2>/dev/null | wc -l | tr -d ' ')"
    # pg_archivecleanup is the supported tool and understands the segment naming
    # rules, including that they do not sort lexically across timeline
    # boundaries. -x .gz tells it the stored files carry that suffix.
    if pg_archivecleanup -x .gz "$ARCHIVE_DIR" "$cutoff" 2>/dev/null; then
      after="$(find "$ARCHIVE_DIR" -maxdepth 1 -type f 2>/dev/null | wc -l | tr -d ' ')"
      echo "  WAL archive: $before -> $after segment(s), keeping everything from $cutoff"
    else
      # Not fatal. A backup that succeeded must not be reported as failed
      # because cleanup did not run; the disk alert is the backstop.
      echo "  WARNING: pruning the WAL archive failed; it will keep growing" >&2
    fi
  else
    echo "  WARNING: could not read a start location from $label; WAL not pruned" >&2
  fi
else
  echo "  WARNING: no base backup found to prune the WAL archive against" >&2
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
