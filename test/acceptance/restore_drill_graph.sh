#!/usr/bin/env bash
# WP-I2 acceptance: restore the WORK GRAPH from the off-machine copy (wg-uh9).
#
#   sudo bash test/acceptance/restore_drill_graph.sh [--evidence FILE]
#
# The two PostgreSQL drills restore the database. Neither of them, and nothing
# else, has ever restored the work graph — from R2 or from the local copy.
#
# That gap is not theoretical. The offsite copy was frozen for two days by the
# bucket lock: it copied the Dolt snapshot directory file-by-file, `manifest` is
# rewritten on every snapshot, and a locked object cannot be overwritten. The
# content-addressed chunks kept uploading, so the object count kept rising and
# the receipt kept refreshing — every signal said the backup was working while
# the restorable state stayed pinned to the day the lock was applied.
#
# Every signal except reading the objects back and rebuilding a graph from them,
# which is what this does. It downloads the newest archive, restores it into a
# throwaway graph, and looks for a marker written moments earlier.
#
# It never touches the live graph except to write and then remove that marker,
# and it restores into a temporary directory, never over /srv/graphs.
set -uo pipefail

BUCKET="${WG_OFFSITE_BUCKET:-workgraph-backups-staging}"
GRAPH="${WG_HQ_GRAPH:-/srv/graphs/company-hq}"
GRAPH_USER="${WG_HQ_GRAPH_USER:-workgraph}"
EVIDENCE=""
# The graph syncs every 15 minutes, the same objective as the database.
RPO_SECONDS=$(( 15 * 60 ))

while [ "$#" -gt 0 ]; do
  case "$1" in
    --evidence) EVIDENCE="$2"; shift 2 ;;
    --bucket)   BUCKET="$2"; shift 2 ;;
    --graph)    GRAPH="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

pass=0; fail=0
ok()  { printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass+1)); }
bad() { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail+1)); }

command -v rclone >/dev/null || { echo "rclone is not installed" >&2; exit 2; }
command -v bd >/dev/null     || { echo "bd is not on PATH" >&2; exit 2; }
[ -d "$GRAPH" ]              || { echo "no work graph at $GRAPH" >&2; exit 2; }

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

WORK="$(mktemp -d /var/tmp/wg-graph-drill.XXXXXX)"
chmod 700 "$WORK"
mkdir -p "$WORK/backup" "$WORK/restored"
chown -R "$GRAPH_USER" "$WORK"

MARKER=""
as_graph_user() { sudo -u "$GRAPH_USER" env HOME="$WORK" "$@"; }

cleanup() {
  # The marker is removed from the LIVE graph whether the drill passed or not.
  # A drill that leaves litter in the backlog is a drill people stop running.
  if [ -n "$MARKER" ]; then
    ( cd "$GRAPH" && sudo -u "$GRAPH_USER" bd delete "$MARKER" --force >/dev/null 2>&1 ) \
      || echo "  could not remove the drill marker $MARKER from the live graph" >&2
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

echo "WP-I2 work-graph restore drill  (r2://$BUCKET/beads-hq/)"
echo

# ---------------------------------------------------------------------------
# A marker, so this proves it restored THIS graph and not merely a graph
# ---------------------------------------------------------------------------
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
MARKER_TITLE="restore drill marker $STAMP"
MARKER="$(cd "$GRAPH" && sudo -u "$GRAPH_USER" bd create "$MARKER_TITLE" \
  --silent --type task --priority 3 2>/dev/null | grep -oE '[a-z]+-[a-z0-9]+' | head -1)"
if [ -z "$MARKER" ]; then
  echo "cannot write a drill marker into the live graph at $GRAPH" >&2; exit 2
fi
echo "  marker $MARKER written at $STAMP"

# A snapshot AND an upload taken after the marker, so the marker is genuinely in
# the copy being restored. Without this the drill would restore this morning's
# archive and correctly fail to find a marker written a minute ago.
echo "  taking a snapshot and pushing it off-machine"
systemctl start wg-backup-beads.service   >/dev/null 2>&1 || true
systemctl start wg-backup-offsite.service >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# Fetch
# ---------------------------------------------------------------------------
echo
echo "Fetching from R2"
FETCH_START=$(date +%s)

NEWEST="$(rclone lsf "r2:$BUCKET/beads-hq/" "${RC[@]}" 2>/dev/null \
          | grep '\.tar\.gz$' | sort | tail -1)"
if [ -z "$NEWEST" ]; then
  # Before the wg-38w fix this prefix held loose Dolt files rather than
  # archives, so an empty result here is a real and specific answer: the
  # off-machine copy is in the layout that could not be updated.
  bad "no work-graph archive in r2://$BUCKET/beads-hq/"
  echo; echo "passed $pass, failed $fail"; exit 1
fi
echo "  newest object: $NEWEST"

if ! rclone copy "r2:$BUCKET/beads-hq/$NEWEST" "$WORK/" "${RC[@]}" >/dev/null 2>&1; then
  bad "could not download $NEWEST"
  echo; echo "passed $pass, failed $fail"; exit 1
fi
DL_BYTES="$(stat -c %s "$WORK/$NEWEST")"
ok "downloaded $NEWEST ($(( DL_BYTES / 1024 )) KB)"

# Integrity before trusting it: a truncated object downloads without complaint.
if tar tzf "$WORK/$NEWEST" >"$WORK/listing" 2>/dev/null; then
  ok "the archive is a readable gzip tar ($(grep -c . "$WORK/listing") entries)"
else
  bad "the archive is not readable"
  echo; echo "passed $pass, failed $fail"; exit 1
fi

if grep -q '^\./manifest$' "$WORK/listing"; then
  ok "the archive contains a manifest, so its chunks can be interpreted"
else
  bad "the archive contains no manifest: the chunks are unreadable without it"
fi

tar xzf "$WORK/$NEWEST" -C "$WORK/backup" || { bad "could not extract the archive"; exit 1; }
chown -R "$GRAPH_USER" "$WORK"
FETCH_SECONDS=$(( $(date +%s) - FETCH_START ))

# ---------------------------------------------------------------------------
# Restore into a graph of its own
# ---------------------------------------------------------------------------
echo
echo "Restoring"
RESTORE_START=$(date +%s)

# bd restores INTO an initialised database, so one is created first. A prefix of
# its own, because this graph is a copy and must never be mistaken for the live
# one by anything reading a bead id out of it.
if ! ( cd "$WORK/restored" && as_graph_user bd init drill ) >"$WORK/init.log" 2>&1; then
  bad "could not initialise a graph to restore into"; sed 's/^/        /' "$WORK/init.log"
  echo; echo "passed $pass, failed $fail"; exit 1
fi
if ! ( cd "$WORK/restored" && as_graph_user bd backup restore "$WORK/backup" --force ) \
      >"$WORK/restore.log" 2>&1; then
  bad "bd backup restore failed"; sed 's/^/        /' "$WORK/restore.log"
  echo; echo "passed $pass, failed $fail"; exit 1
fi
RESTORE_SECONDS=$(( $(date +%s) - RESTORE_START ))
ok "restored in ${RESTORE_SECONDS}s"

# ---------------------------------------------------------------------------
# Did it restore THIS graph?
# ---------------------------------------------------------------------------
echo
echo "Checking the restored graph"

RESTORED_TITLE="$(cd "$WORK/restored" && as_graph_user bd show "$MARKER" 2>/dev/null | head -3)"
if printf '%s' "$RESTORED_TITLE" | grep -qF "$STAMP"; then
  ok "the marker survived the round trip through R2"
else
  # This is the assertion the two-day freeze would have failed, every run, from
  # the day the lock was applied.
  bad "the marker $MARKER is NOT in the copy restored from R2; the off-machine graph is stale"
fi

count_issues() { grep -oE 'Total Issues: *[0-9]+' | grep -oE '[0-9]+'; }
LIVE_N="$( ( cd "$GRAPH" && sudo -u "$GRAPH_USER" bd stats 2>/dev/null ) | count_issues )"
REST_N="$( ( cd "$WORK/restored" && as_graph_user bd stats 2>/dev/null ) | count_issues )"
if [ -n "$LIVE_N" ] && [ "$LIVE_N" = "$REST_N" ]; then
  ok "the restored graph holds the same $LIVE_N issues as the live one"
else
  bad "issue counts differ: live ${LIVE_N:-unknown}, restored ${REST_N:-unknown}"
fi

# ---------------------------------------------------------------------------
# Objective
# ---------------------------------------------------------------------------
echo
echo "Objective"
# The archive name IS its timestamp, which is what makes the recovery point
# readable without opening it.
ARCHIVE_AT="${NEWEST%.tar.gz}"
AGE=$(( $(date +%s) - $(date -u -d "${ARCHIVE_AT:0:8} ${ARCHIVE_AT:9:2}:${ARCHIVE_AT:11:2}:${ARCHIVE_AT:13:2} UTC" +%s 2>/dev/null || echo 0) ))
if [ "$AGE" -gt 0 ] && [ "$AGE" -le "$RPO_SECONDS" ]; then
  ok "recovery point ${AGE}s, within the documented ${RPO_SECONDS}s"
else
  bad "recovery point ${AGE}s exceeds the documented ${RPO_SECONDS}s"
fi

echo
echo "----------------------------------------"
printf 'passed %d, failed %d\n' "$pass" "$fail"

if [ -n "$EVIDENCE" ]; then
  cat > "$EVIDENCE" <<JSON
{
  "drill": "work-graph-restore-from-offsite",
  "work_package": "WP-I2",
  "bead": "wg-uh9",
  "at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "host": "$(hostname)",
  "source": "r2://$BUCKET/beads-hq/$NEWEST",
  "downloaded_bytes": $DL_BYTES,
  "marker": "$MARKER",
  "marker_written_at": "$STAMP",
  "live_issues": ${LIVE_N:-null},
  "restored_issues": ${REST_N:-null},
  "fetch_seconds": $FETCH_SECONDS,
  "restore_seconds": $RESTORE_SECONDS,
  "recovery_point_seconds": $AGE,
  "rpo_seconds_documented": $RPO_SECONDS,
  "read_local_backups": false,
  "note": "Restored entirely from one archive downloaded out of R2 into a throwaway graph. /var/lib/workgraph/backups/beads was not read, and /srv/graphs was never written.",
  "checks_passed": $pass,
  "checks_failed": $fail
}
JSON
  echo "evidence written to $EVIDENCE"
fi

[ "$fail" -eq 0 ] || exit 1
