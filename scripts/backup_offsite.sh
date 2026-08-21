#!/usr/bin/env bash
# Copy backups off the machine, to R2 (WP-I2).
#
#   backup_offsite.sh [--bucket NAME] [--dry-run]
#
# Run as root from a systemd timer, every 15 minutes. Until this existed every
# copy lived on the same disk as the thing it protected, which survives a bad
# migration and a dropped table and does not survive losing the node.
#
# THIS SCRIPT NEVER DELETES FROM R2.
#
# That is the central decision here, not an omission. The node holds a token that
# can write to the backup bucket, so if the node is compromised, anything the node
# can delete is not a backup — an attacker who can reach the disk can also reach
# the copies of it. Retention off-machine is therefore a SERVER-SIDE lifecycle
# rule on the bucket, which the node cannot influence at all.
#
# The cost of never deleting is small and known: a base backup tars to ~19 MB, so
# 30 a month is about 570 MB and a year is under 7 GB, against R2's 10 GB free
# tier. Cheap enough that the safe choice is also the affordable one.
set -uo pipefail

BUCKET="${WG_OFFSITE_BUCKET:-workgraph-backups-staging}"
BASE_DIR="${WG_BACKUP_BASE_DIR:-/var/lib/workgraph/backups/postgres}"
ARCHIVE_DIR="${WG_WAL_ARCHIVE_DIR:-/var/lib/workgraph/wal-archive}"
BEADS_DIR="${WG_HQ_BACKUP_DIR:-/var/lib/workgraph/backups/beads}"
RECEIPT_DIR="${WG_BACKUP_RECEIPT_DIR:-/var/lib/workgraph/backups}"
DRY_RUN=0

while [ "$#" -gt 0 ]; do
  case "$1" in
    --bucket)  BUCKET="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

command -v rclone >/dev/null || { echo "rclone is not installed" >&2; exit 2; }

# Credentials from the systemd credential store, falling back to the environment
# for a manual run. Read into variables and never written to a config file: an
# rclone.conf on disk would be a second copy of the credential with its own
# lifetime, which is what the SOPS work exists to avoid.
read_credential() {
  local name="$1" envvar="$2"
  if [ -n "${CREDENTIALS_DIRECTORY:-}" ] && [ -r "$CREDENTIALS_DIRECTORY/$name" ]; then
    tr -d '\r\n' < "$CREDENTIALS_DIRECTORY/$name"
  else
    printf '%s' "${!envvar:-}"
  fi
}

ACCESS_KEY="$(read_credential r2_access_key_id R2_ACCESS_KEY_ID)"
SECRET_KEY="$(read_credential r2_secret_access_key R2_SECRET_ACCESS_KEY)"
ENDPOINT="${R2_S3_ENDPOINT:-}"
if [ -z "$ENDPOINT" ]; then
  ACCOUNT="$(read_credential r2_account_id CLOUDFLARE_ACCOUNT_ID)"
  [ -n "$ACCOUNT" ] && ENDPOINT="https://${ACCOUNT}.r2.cloudflarestorage.com"
fi

if [ -z "$ACCESS_KEY" ] || [ -z "$SECRET_KEY" ] || [ -z "$ENDPOINT" ]; then
  echo "no R2 credentials available; cannot copy backups off the machine" >&2
  echo "expected r2_access_key_id, r2_secret_access_key and an endpoint" >&2
  exit 1
fi

export RCLONE_CONFIG_R2_TYPE=s3
export RCLONE_CONFIG_R2_PROVIDER=Cloudflare
export RCLONE_CONFIG_R2_ACCESS_KEY_ID="$ACCESS_KEY"
export RCLONE_CONFIG_R2_SECRET_ACCESS_KEY="$SECRET_KEY"
export RCLONE_CONFIG_R2_ENDPOINT="$ENDPOINT"
# The token deliberately cannot create buckets, so rclone must not try. Without
# this it issues CreateBucket before every upload and fails with AccessDenied on
# a bucket that exists and is writable.
#
# --checksum, not the default modtime comparison. R2 does not implement the
# CopyObject call rclone uses to update a modification time, so a second run over
# objects that already exist fails with "NotImplemented: Not Implemented" (501)
# on every one of them. Comparing by checksum skips that path entirely and is a
# stronger check anyway.
# --no-update-modtime as well: R2 does not implement the CopyObject call rclone
# uses to rewrite a modification time, so without it a re-run over objects that
# already exist logs a 501 for each one.
RC=(--s3-no-check-bucket --checksum --no-update-modtime --retries 3 --low-level-retries 5)
[ "$DRY_RUN" = 1 ] && RC+=(--dry-run)

echo "offsite backup -> r2://$BUCKET"

fail=0

# ---------------------------------------------------------------------------
# Base backups: ONE TAR EACH
# ---------------------------------------------------------------------------
#
# A plain base backup is about 2,150 files. Uploaded as a directory that is 2,150
# operations per backup, ~65,000 a month, for 129 MB. Tarred it is ONE operation
# for 19 MB — 6.8x less storage and two orders of magnitude fewer requests. On R2
# the requests are what eventually cost money, not the bytes.
uploaded=0
skipped=0
for run in $(find "$BASE_DIR" -maxdepth 1 -mindepth 1 -type d -printf '%f\n' | sort); do
  remote="r2:$BUCKET/postgres/$run.tar.gz"

  # Already there? Never re-upload: these are immutable once written, and
  # re-uploading would overwrite an object that a lifecycle rule may be
  # protecting.
  #
  # Test the OUTPUT, not the exit status. `rclone lsf` on a path that does not
  # exist exits 0 with nothing printed — it treats it as an empty directory — so
  # checking the exit status reported every backup as "already present" and
  # uploaded nothing, while saying it had. The only reason that did not become a
  # silent total failure is that the receipt is written from a separate count of
  # what is actually in the bucket, which stayed at zero and refused to claim a
  # copy existed.
  if [ -n "$(rclone lsf "$remote" "${RC[@]}" 2>/dev/null)" ]; then
    skipped=$((skipped + 1))
    continue
  fi

  if [ "$DRY_RUN" = 1 ]; then
    echo "  would upload $run"
    uploaded=$((uploaded + 1))
    continue
  fi

  # Tar to a temporary FILE, then upload it. Streaming through `rclone rcat` was
  # the obvious version and R2 rejects it: with an unknown length rclone uses a
  # multipart flow R2 does not implement, and every upload failed with
  # "Post request rcat error: NotImplemented" (501). With the size known, a
  # 19 MB object goes up as a single PUT.
  #
  # The scratch space is the reason it was written the other way round, and it is
  # not a real cost: 19 MB against 141 GB free. Knowing the size also allows the
  # exact size comparison below, which is a stronger check than "not empty".
  tmp="$(mktemp)" || { echo "  cannot create a temporary file" >&2; fail=1; continue; }
  if ! tar czf "$tmp" -C "$BASE_DIR/$run" .; then
    echo "  failed to archive $run" >&2
    rm -f "$tmp"; fail=1; continue
  fi
  local_size="$(stat -c %s "$tmp")"

  if rclone copyto "$tmp" "$remote" "${RC[@]}"; then
    # Read the size back from the bucket and require it to MATCH. An upload that
    # reports success and writes a truncated object is the failure a backup
    # cannot afford, and comparing bytes rules it out for the cost of one HEAD.
    remote_size="$(rclone size "$remote" "${RC[@]}" --json 2>/dev/null \
                   | python3 -c 'import json,sys; print(json.load(sys.stdin).get("bytes",0))' 2>/dev/null)"
    if [ "${remote_size:-0}" = "$local_size" ]; then
      echo "  uploaded $run ($(( local_size / 1048576 )) MB)"
      uploaded=$((uploaded + 1))
    else
      echo "  SIZE MISMATCH for $run: local $local_size, remote ${remote_size:-0}" >&2
      fail=1
    fi
  else
    echo "  failed to upload $run" >&2
    fail=1
  fi
  rm -f "$tmp"
done
echo "  base backups: $uploaded uploaded, $skipped already present"

# ---------------------------------------------------------------------------
# One copy per calendar month, under its own prefix
# ---------------------------------------------------------------------------
#
# The plan asks for thirty daily and twelve monthly (section 15.3). A single
# age-based lifecycle rule cannot express both, so the monthly copies live under
# their own prefix with their own retention — 400 days against 35 for the
# dailies. Without this, "twelve monthly" would be a sentence in a document and
# nothing else.
#
# Uploaded rather than server-side copied: R2 rejects several of rclone's
# server-side copy paths with 501, and one extra 19 MB PUT a month is not worth
# the fragility of finding out which.
newest_local="$(find "$BASE_DIR" -maxdepth 1 -mindepth 1 -type d -printf '%f\n' | sort | tail -1)"
if [ -n "$newest_local" ]; then
  month="${newest_local:0:6}"   # YYYYMM from a YYYYMMDDTHHMMSSZ stamp
  monthly="r2:$BUCKET/postgres-monthly/$month.tar.gz"
  if [ -n "$(rclone lsf "$monthly" "${RC[@]}" 2>/dev/null)" ]; then
    echo "  monthly copy for $month already present"
  elif [ "$DRY_RUN" = 1 ]; then
    echo "  would create the monthly copy for $month"
  else
    tmp="$(mktemp)"
    if tar czf "$tmp" -C "$BASE_DIR/$newest_local" . \
       && rclone copyto "$tmp" "$monthly" "${RC[@]}"; then
      echo "  monthly copy created for $month (from $newest_local)"
    else
      echo "  failed to create the monthly copy for $month" >&2
      fail=1
    fi
    rm -f "$tmp"
  fi
fi

# ---------------------------------------------------------------------------
# WAL: the part that actually determines the off-machine recovery point
# ---------------------------------------------------------------------------
#
# If the node is lost, the recovery point is decided by the newest WAL segment
# that reached R2 — not by the newest one in the local archive, which died with
# the node. So this runs every 15 minutes, and the segments are already gzipped
# and about 22 KB each, so a sync is cheap.
#
# copy, not sync: sync deletes remote files that have gone locally, and local
# pruning removes segments as base backups age out. That would propagate local
# retention into the off-machine copy and delete the only remaining copy of old
# WAL.
if rclone copy "$ARCHIVE_DIR" "r2:$BUCKET/wal/" "${RC[@]}" --transfers 8; then
  n="$(rclone lsf "r2:$BUCKET/wal/" "${RC[@]}" 2>/dev/null | wc -l | tr -d ' ')"
  echo "  WAL segments off-machine: $n"
else
  echo "  failed to sync the WAL archive" >&2
  fail=1
fi

# ---------------------------------------------------------------------------
# The work graph
# ---------------------------------------------------------------------------
if [ -d "$BEADS_DIR" ] && [ -n "$(ls -A "$BEADS_DIR" 2>/dev/null)" ]; then
  if rclone copy "$BEADS_DIR" "r2:$BUCKET/beads-hq/" "${RC[@]}"; then
    echo "  work graph copied off-machine"
  else
    echo "  failed to copy the work graph" >&2
    fail=1
  fi
else
  echo "  no work-graph snapshot to copy yet" >&2
fi

[ "$DRY_RUN" = 1 ] && exit 0

# ---------------------------------------------------------------------------
# Receipts, only for what is actually off-machine now
# ---------------------------------------------------------------------------
#
# Separate receipts from the local ones, because they answer a different
# question. postgres.receipt says a backup exists; postgres-offsite.receipt says
# it would survive losing this machine. Conflating them would let a local backup
# silence the alert about there being no remote copy.
if [ "$fail" = 0 ] && [ -d "$RECEIPT_DIR" ] && [ -w "$RECEIPT_DIR" ]; then
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  # Only claim the postgres stream if a base backup is genuinely present remotely.
  pg_objects="$(rclone lsf "r2:$BUCKET/postgres/" "${RC[@]}" 2>/dev/null | wc -l | tr -d ' ')"
  bd_objects="$(rclone lsf "r2:$BUCKET/beads-hq/" "${RC[@]}" 2>/dev/null | wc -l | tr -d ' ')"
  if [ "${pg_objects:-0}" -gt 0 ]; then
    printf '%s\n' "$now" > "$RECEIPT_DIR/postgres-offsite.receipt"
  else
    echo "  no PostgreSQL backup is present in r2://$BUCKET/postgres/;" >&2
    echo "  not writing a receipt for a copy that does not exist" >&2
    fail=1
  fi
  if [ "${bd_objects:-0}" -gt 0 ]; then
    printf '%s\n' "$now" > "$RECEIPT_DIR/beads-offsite.receipt"
  fi
  echo "  off-machine: $pg_objects postgres object(s), $bd_objects work-graph object(s)"
  echo "  receipts written"
fi

exit "$fail"
