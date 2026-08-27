#!/usr/bin/env bash
# Fail when the Ansible defaults disagree with versions.lock.
#
#   scripts/check_pins.sh
#
# versions.lock is the authority for what a node runs. Ansible reads its own
# defaults, which are a hand-copy of it — so bumping the lock and forgetting the
# copy deploys the OLD version while the repository claims the new one, and the
# deploy reports success either way. Nothing checked this until wg-ba9 added a
# fifth pair to keep in step.
#
# Comparing the files rather than the node on purpose: a node can be checked only
# by reaching it, and this has to fail in CI, before the deploy that would have
# been wrong.
set -uo pipefail

cd "$(dirname "$0")/.."
source scripts/lockfile.sh

DEFAULTS="infra/ansible/roles/gastown/defaults/main.yml"
fail=0

# ansible_value <key> -> the scalar for that key, quotes stripped
ansible_value() {
  awk -v key="$1" '
    $0 ~ "^" key ":" {
      sub("^" key ": *", ""); gsub(/^"/, ""); gsub(/"$/, ""); print; exit
    }
  ' "$DEFAULTS"
}

compare() {
  local what="$1" want="$2" got="$3"
  if [ -z "$want" ]; then
    echo "  MISSING  $what is not in versions.lock"
    fail=1
  elif [ -z "$got" ]; then
    echo "  MISSING  $what is not in $DEFAULTS"
    fail=1
  elif [ "$want" != "$got" ]; then
    echo "  DRIFT    $what: versions.lock says $want, Ansible says $got"
    fail=1
  else
    echo "  ok       $what $want"
  fi
}

echo "pins (versions.lock vs Ansible):"
compare "gastown version" "$(lock_version gastown)" "$(ansible_value gastown_version)"
compare "beads version"   "$(lock_version beads)"   "$(ansible_value beads_version)"
compare "dolt version"    "$(lock_version dolt)"    "$(ansible_value dolt_version)"
compare "opencode version" "$(lock_version opencode)" "$(ansible_value opencode_version)"
compare "claude-code version" "$(lock_version claude_code)" "$(ansible_value claude_code_version)"

compare "gastown sha256"  "$(lock_sha gastown linux_amd64)"  "$(ansible_value gastown_sha256_linux_amd64)"
compare "beads sha256"    "$(lock_sha beads linux_amd64)"    "$(ansible_value beads_sha256_linux_amd64)"
compare "dolt sha256"     "$(lock_sha dolt linux_amd64)"     "$(ansible_value dolt_sha256_linux_amd64)"
compare "opencode sha256" "$(lock_sha opencode linux_amd64)" "$(ansible_value opencode_sha256_linux_amd64)"

if [ "$fail" -ne 0 ]; then
  echo
  echo "versions.lock is the authority. Update $DEFAULTS to match it." >&2
  exit 1
fi
echo "every pin matches versions.lock"
