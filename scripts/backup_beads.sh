#!/usr/bin/env bash
# Back up the work graph off this machine.
#
# The bead history is the audit trail for how this system was built: every
# closure carries the evidence it was closed on. Until now it existed in exactly
# one place — a Dolt database in a working copy — and the reviewable export in
# company-workgraph had fallen four phases behind, so neither half of the
# documented recovery path was being maintained.
#
# bd export is NOT a backup. Its own help says so: it does not produce the
# snapshot bd backup restore consumes, and JSONL preserves neither Dolt history
# nor state. This script therefore does both, and they are different things:
#
#   bd backup sync   the restorable snapshot, with history
#   bd export        the reviewable structure, for a pull request
#
# DoltHub is bd's recommended cloud destination and is deliberately not used:
# this graph contains restricted client work, and putting it on a third-party
# service is a disclosure decision nobody has made.
set -euo pipefail

: "${R2_ACCESS_KEY_ID:?source ~/.config/datopian-workgraph/credentials.env}"
: "${R2_SECRET_ACCESS_KEY:?}"
: "${R2_S3_ENDPOINT:?}"

BUCKET="${WG_BACKUP_BUCKET:-workgraph-backups-staging}"

# The R2 credential must cover this bucket, and at the time of writing it does
# not: it was minted before Terraform created the backup, evidence and audit
# buckets, so it reaches workgraph-tfstate-staging and 403s on everything else.
# Checked up front, because the failure otherwise appears as rclone retrying a
# CreateBucket it has no business attempting, which reads as a tooling problem
# rather than a permissions one.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCAL="${WG_BACKUP_DIR:-$HOME/.local/share/workgraph/beads-backup}"

cd "$ROOT"
export PATH="$ROOT/.toolchain/bin:$PATH"

mkdir -p "$LOCAL"

# Always point the destination at $LOCAL, rather than only when none is set.
#
# bd auto-detects a destination from the git remote, so `bd backup status`
# succeeds while pointing somewhere this script never reads — the first run
# skipped init on that basis and then failed to sync, which read as "backup
# broken" when it was "backup configured elsewhere". init is safe to repeat.
bd backup init "$LOCAL" >/dev/null 2>&1 || true

# rclone rather than a hand-rolled SigV4 request: R2 is S3-compatible, and the
# signing is the part that goes wrong silently.
export RCLONE_CONFIG_R2_TYPE=s3
export RCLONE_CONFIG_R2_PROVIDER=Cloudflare
export RCLONE_CONFIG_R2_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID"
export RCLONE_CONFIG_R2_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY"
export RCLONE_CONFIG_R2_ENDPOINT="$R2_S3_ENDPOINT"
# R2 has no regions, but the S3 client insists on one.
export RCLONE_CONFIG_R2_REGION=auto

if ! rclone lsf "r2:$BUCKET" --s3-no-check-bucket --max-depth 1 >/dev/null 2>&1; then
  echo "  the R2 credential cannot reach r2://$BUCKET" >&2
  echo "  mint an R2 token with Object Read & Write covering that bucket and set" >&2
  echo "  R2_ACCESS_KEY_ID and R2_SECRET_ACCESS_KEY; the local snapshot below still runs" >&2
  SKIP_UPLOAD=1
fi

echo "  syncing the Dolt snapshot to $LOCAL"
bd backup sync >/dev/null 2>&1 || {
  echo "  bd backup sync failed" >&2
  exit 1
}

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"

# A dated copy, never an overwriting one.
#
# A backup that mirrors the live graph faithfully also mirrors its corruption
# faithfully, and the failure that most needs recovery — a bad write, a
# mistaken bulk close — is exactly the one a mirror destroys the evidence of.
if [ "${SKIP_UPLOAD:-0}" = "1" ]; then
  echo "  LOCAL SNAPSHOT ONLY — nothing was copied off this machine" >&2
  exit 2
fi

echo "  uploading to r2://$BUCKET/beads/$STAMP/"
rclone copy "$LOCAL" "r2:$BUCKET/beads/$STAMP/" --checksum --quiet

# Verify by reading back, not by trusting the exit code. An upload that reports
# success and wrote nothing is the failure mode a backup cannot afford.
COUNT=$(rclone size "r2:$BUCKET/beads/$STAMP/" --json 2>/dev/null \
  | python3 -c 'import json,sys; print(json.load(sys.stdin).get("count", 0))')
if [ "${COUNT:-0}" -eq 0 ]; then
  echo "  the upload reported success but the destination is empty" >&2
  exit 1
fi
echo "  verified: $COUNT object(s) at beads/$STAMP/"

# The reviewable export, refreshed alongside so the two halves stay together.
SEED="${WG_SEED_PATH:-$ROOT/../company-workgraph/beads-bootstrap/seed/go-live-plan.jsonl}"
if [ -d "$(dirname "$SEED")" ]; then
  bd export -o "$SEED" >/dev/null 2>&1
  echo "  refreshed the reviewable export at $SEED"
  echo "  commit it in company-workgraph; it is not a backup, it is the shape of the plan"
fi
