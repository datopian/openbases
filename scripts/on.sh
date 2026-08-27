#!/usr/bin/env bash
# Run a command on a node.
#
#   scripts/on.sh staging control 'wg-env wg-work list oss'
#   scripts/on.sh staging execution 'journalctl -u wg-dispatcher-oss -n 50 --no-pager'
#
# Plain `ssh ssh-staging.openbases.com` does not work unless your ssh config
# already carries the cloudflared ProxyCommand and an Access service token —
# neither node has an inbound port. Ansible's config has both, so this borrows
# them rather than asking everyone to set up a second copy.
#
# Read-mostly by design: it runs one command and prints what came back. For
# anything that changes a node, write the change into a role instead, so the
# next deploy does not quietly undo it.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENVIRONMENT="${1:?usage: on.sh <staging|production> <control|execution> <command>}"
ROLE="${2:?usage: on.sh <environment> <control|execution> <command>}"
shift 2
[ "$#" -gt 0 ] || { echo "usage: on.sh <environment> <control|execution> <command>" >&2; exit 2; }

case "$ROLE" in
  control|execution) ;;
  *) echo "role must be 'control' or 'execution', got '$ROLE'" >&2; exit 2 ;;
esac

cd "$ROOT/infra/ansible"
exec "$ROOT/scripts/with_secrets.sh" "$ENVIRONMENT" \
  ansible "workgraph-${ENVIRONMENT}-${ROLE}" \
  -i "inventory/${ENVIRONMENT}.yml" \
  --become -m shell -a "$*"
