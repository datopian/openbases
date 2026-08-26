#!/usr/bin/env bash
# Fail when the committed work-graph export no longer matches the live graph.
#
#   scripts/check_seed_current.sh [--fix]
#
# beads-bootstrap/seed/go-live-plan.jsonl in the company-workgraph repository is
# the reviewable shape of the plan: the work breakdown and the dependency edges,
# readable in a pull request by someone with no access to a running Beads.
#
# Its README says "a CI guard fails when the committed export no longer matches
# the graph, so drifting is visible rather than quiet". There was no such guard.
# The export fell four days and thirteen beads behind — including every finding
# recorded during that time — and nothing said so. This is that guard.
#
# It cannot run in CI, and the README claiming it did was the actual problem.
# CI has no Beads graph: the graph lives in a working copy, and reconstructing it
# from the export in order to compare it against the export proves nothing. So
# this runs where the graph is — a workstation — and is called by
# scripts/backup_beads.sh, which is the routine that already touches both halves.
#
# --fix refreshes the export instead of only reporting, so the answer to a
# failure is one command rather than a remembered incantation.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SEED="${WG_SEED_PATH:-$ROOT/../company-workgraph/beads-bootstrap/seed/go-live-plan.jsonl}"
FIX=0
LOCAL=0
for arg in "$@"; do
  case "$arg" in
    --fix)   FIX=1 ;;
    --local) LOCAL=1 ;;
    *) echo "usage: check_seed_current.sh [--fix] [--local]" >&2; exit 2 ;;
  esac
done

# The company graph lives on the control node (wg-22k), so that is what the
# committed export must match. --local compares a working copy instead, which is
# only meaningful while one is still being used as a source of truth.
if [ "$LOCAL" = 1 ]; then
  command -v bd >/dev/null || { echo "bd is not on PATH" >&2; exit 2; }
fi
if [ ! -d "$(dirname "$SEED")" ]; then
  # Not an error: the implementation repository is often cloned without the
  # planning repository beside it, and failing there would make this check
  # something people learn to skip.
  echo "no seed directory at $(dirname "$SEED"); nothing to check"
  exit 0
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

if [ "$LOCAL" = 1 ]; then
  if ! bd export -o "$tmp" >/dev/null 2>&1; then
    echo "bd export failed; cannot tell whether the seed is current" >&2
    exit 2
  fi
else
  # Exported on the node and copied back, rather than streamed: bd export writes
  # a file and reports, and -o /dev/stdout produces nothing.
  remote="/tmp/wg-seed-check.$$.jsonl"
  if ! "$ROOT/scripts/hq.sh" export -o "$remote" >/dev/null 2>&1; then
    echo "could not export the company graph from the control node" >&2
    echo "if the node is unreachable, compare a working copy with --local" >&2
    exit 2
  fi
  ssh -o ProxyCommand="cloudflared access ssh --hostname %h" \
      -o StrictHostKeyChecking=no -o ConnectTimeout=30 \
      "root@${WG_CONTROL_HOST:-ssh-staging.openbases.com}" \
      "cat $remote; rm -f $remote" > "$tmp" 2>/dev/null
  if [ ! -s "$tmp" ]; then
    echo "the export from the control node came back empty" >&2
    exit 2
  fi
fi

if [ ! -f "$SEED" ]; then
  echo "the committed export is missing entirely: $SEED" >&2
  [ "$FIX" = 1 ] && { cp "$tmp" "$SEED"; echo "written; commit it in company-workgraph"; exit 0; }
  exit 1
fi

if diff -q "$SEED" "$tmp" >/dev/null 2>&1; then
  echo "the committed export matches the live graph ($(grep -c . "$tmp") beads)"
  exit 0
fi

if [ "$FIX" = 1 ]; then
  cp "$tmp" "$SEED"
  echo "refreshed $SEED ($(grep -c . "$tmp") beads); commit it in company-workgraph"
  exit 0
fi

# Say WHICH beads, not just that something differs. "the export is stale" gets
# refreshed; "these six findings exist only on this machine" gets committed.
python3 - "$SEED" "$tmp" <<'PY'
import json, sys

def ids(path):
    out = {}
    for line in open(path):
        line = line.strip()
        if not line:
            continue
        try:
            d = json.loads(line)
        except ValueError:
            continue
        if "id" in d:
            out[d["id"]] = d
    return out

committed, live = ids(sys.argv[1]), ids(sys.argv[2])
missing = sorted(set(live) - set(committed))
gone = sorted(set(committed) - set(live))
changed = sorted(i for i in set(live) & set(committed) if live[i] != committed[i])

print("the committed export no longer matches the live graph:", file=sys.stderr)
if missing:
    print(f"  {len(missing)} bead(s) exist only in the live graph:", file=sys.stderr)
    for i in missing:
        print(f"    {i}  {live[i].get('title','')[:70]}", file=sys.stderr)
if gone:
    print(f"  {len(gone)} bead(s) are in the export but not the graph: {', '.join(gone)}", file=sys.stderr)
if changed:
    print(f"  {len(changed)} bead(s) differ: {', '.join(changed[:12])}"
          + (" …" if len(changed) > 12 else ""), file=sys.stderr)
PY

echo >&2
echo "refresh it with: scripts/check_seed_current.sh --fix" >&2
echo "then commit beads-bootstrap/seed/go-live-plan.jsonl in company-workgraph" >&2
exit 1
