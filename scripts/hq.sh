#!/usr/bin/env bash
# Run bd against the company work graph, which lives on the control node.
#
#   scripts/hq.sh list --status open
#   scripts/hq.sh show wg-2a0
#   scripts/hq.sh create "A title with spaces" --type task --priority 1
#
# The graph is on the node and not on a laptop, which is the whole point of
# wg-22k: the backlog used to exist in one working copy whose only backup was
# another directory on the same machine. On the node it is snapshotted every
# fifteen minutes, copied to R2, restore-drilled (wg-uh9) and monitored.
#
# The cost is this indirection. There is no cheaper way today: bd can talk to a
# remote Dolt sql-server, and exposing MySQL through the tunnel is a larger piece
# of infrastructure than the problem justifies, so this runs bd where the graph
# is and brings the output back.
#
# Every argument is quoted with printf %q before it crosses the ssh boundary,
# because ssh concatenates its arguments into ONE string and hands them to a
# remote shell. Without quoting, `hq.sh create "Two words"` creates an issue
# called "Two" and passes "words" as a flag — and titles with an apostrophe,
# which is most of them, break the command outright.
set -uo pipefail

HOST="${WG_CONTROL_HOST:-ssh-staging.openbases.com}"
GRAPH="${WG_HQ_GRAPH:-/srv/graphs/company-hq}"
USER_ON_NODE="${WG_HQ_GRAPH_USER:-workgraph}"

[ "$#" -gt 0 ] || {
  echo "usage: hq.sh <bd-command> [args...]     e.g. hq.sh list --status open" >&2
  exit 2
}

quoted=""
for arg in "$@"; do
  quoted+=" $(printf '%q' "$arg")"
done

# HOME points at the graph directory, exactly as the beads_hq role does for
# every bd invocation it makes (roles/beads_hq/tasks/graph.yml).
#
# Without it, `sudo -u workgraph bd` runs with root's HOME. Dolt writes its
# configuration under $HOME, and on this path it rewrote
# .beads/embeddeddolt/wg/.dolt/noms/manifest as ROOT — after which the service
# user could not read its own work graph. That happened twice on 3 September:
# once found only because wg-backup-beads.service failed, and again after a
# batch of label edits through this script. Both times the repair was a full
# playbook run.
#
# So this is not tidiness. It is the difference between a script that reads the
# graph and a script that breaks it for every service on the node.
exec ssh -o ProxyCommand="cloudflared access ssh --hostname %h" \
         -o StrictHostKeyChecking=no -o ConnectTimeout=30 \
         "root@${HOST}" \
         "cd $(printf '%q' "$GRAPH") && sudo -u $(printf '%q' "$USER_ON_NODE") env HOME=$(printf '%q' "$GRAPH") bd${quoted}"
