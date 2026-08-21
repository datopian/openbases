#!/usr/bin/env bash
# Back up the company HQ work graph on the control node (WP-I2).
#
#   backup_beads_hq.sh [--graph DIR] [--dest DIR] [--export] [--receipt-dir DIR]
#
# Run as the graph's owner, from a systemd timer: every 15 minutes for the native
# snapshot, daily for the JSONL export. The plan's 15-minute recovery point
# objective applies to the work graph as much as to PostgreSQL.
#
# This replaces the workstation script (scripts/backup_beads.sh) for the graph
# that lives on the node. The distinction matters: wg-ohk observed that the graph
# existed only in a working copy on one laptop, backed up when somebody
# remembered. WP-D2 put the company graph on the control node, which is where a
# scheduled job can reach it.
#
# `bd backup sync` and NOT `bd export`. Beads' own help says the JSONL export is
# not a backup: it preserves neither Dolt history nor the state that
# `bd backup restore` consumes. The export is taken as well, because a
# human-readable snapshot is a genuinely useful second recovery aid — but it is
# the second one, and calling it the backup is how a restore fails.
set -uo pipefail

GRAPH="${WG_HQ_GRAPH:-/srv/graphs/company-hq}"
DEST="${WG_HQ_BACKUP_DIR:-/var/lib/workgraph/backups/beads}"
RECEIPT_DIR="${WG_BACKUP_RECEIPT_DIR:-/var/lib/workgraph/backups}"
DO_EXPORT=0

while [ "$#" -gt 0 ]; do
  case "$1" in
    --graph)       GRAPH="$2"; shift 2 ;;
    --dest)        DEST="$2"; shift 2 ;;
    --receipt-dir) RECEIPT_DIR="$2"; shift 2 ;;
    --export)      DO_EXPORT=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

command -v bd >/dev/null || { echo "bd is not on PATH" >&2; exit 2; }
[ -d "$GRAPH" ] || { echo "no graph at $GRAPH" >&2; exit 1; }

cd "$GRAPH" || exit 1
mkdir -p "$DEST" || { echo "cannot create $DEST" >&2; exit 1; }

echo "work graph backup: $GRAPH -> $DEST"

# init is idempotent and required before the first sync. It records the
# destination in the graph's own configuration, so re-running is a no-op once
# it is set.
bd backup init "$DEST" >/dev/null 2>&1 || true

if ! bd backup sync 2>&1 | sed 's/^/  /'; then
  echo "  bd backup sync failed" >&2
  exit 1
fi

# Verify by asking Beads, not by trusting the exit code. `bd backup status`
# reports the destination and what it holds; an empty destination after a
# successful-looking sync is the failure a backup cannot afford.
status="$(bd backup status 2>&1)"
echo "$status" | sed 's/^/  /'

if [ -z "$(ls -A "$DEST" 2>/dev/null)" ]; then
  echo "  the sync reported success and the destination is EMPTY" >&2
  exit 1
fi

if [ "$DO_EXPORT" = 1 ]; then
  # Dated, so the daily exports form a history rather than one file that is
  # overwritten — the overwrite would mean a corrupted graph destroys every
  # export on its next run.
  out="$DEST/export-$(date -u +%Y%m%d).jsonl"
  if bd export -o "$out" >/dev/null 2>&1; then
    echo "  JSONL export: $out ($(wc -l < "$out" | tr -d ' ') line(s))"
  else
    # A failed export is not a failed backup. The native snapshot above is the
    # restorable artefact; this is the additional aid.
    echo "  the JSONL export failed; the native snapshot above is unaffected" >&2
  fi
fi

# The receipt, only after the destination was confirmed non-empty.
if [ -d "$RECEIPT_DIR" ] && [ -w "$RECEIPT_DIR" ]; then
  date -u +%Y-%m-%dT%H:%M:%SZ > "$RECEIPT_DIR/beads.receipt"
  echo "  receipt written to $RECEIPT_DIR/beads.receipt"
else
  echo "  no writable receipt directory at $RECEIPT_DIR" >&2
  exit 1
fi
