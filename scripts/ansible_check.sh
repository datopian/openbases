#!/usr/bin/env bash
# Apply the hardening playbook, then prove it is idempotent.
#
# WP-B2 acceptance: "a second Ansible run produces no unexpected changes".
# Asserting that by hand is unreliable, so this runs it twice and fails if the
# second run would change anything.
set -euo pipefail

cd "$(dirname "$0")/../infra/ansible"

ENV="${1:-staging}"
INV="inventory/${ENV}.yml"
[ -f "$INV" ] || { echo "no inventory for '$ENV'" >&2; exit 2; }

echo "==> pass 1: apply"
ansible-playbook -i "$INV" site.yml

echo
echo "==> pass 2: check mode, expecting zero changes"
out=$(ansible-playbook -i "$INV" site.yml --check --diff 2>&1) || {
  echo "$out"
  echo "check run failed" >&2
  exit 1
}
echo "$out" | tail -20

# The recap line reads "changed=N"; anything above zero means the first run did
# not converge, which is what idempotence means here.
if echo "$out" | grep -qE 'changed=[1-9]'; then
  echo
  echo "NOT IDEMPOTENT — the second run still reports changes:" >&2
  echo "$out" | grep -E '^changed:|changed=[1-9]' >&2
  exit 1
fi

echo
echo "idempotent: second run reports no changes"
