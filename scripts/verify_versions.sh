#!/usr/bin/env bash
# Verify that the gt/bd/dolt on PATH match versions.lock exactly.
#
# The matrix moves as a unit. Gas Town v1.2.0 refuses to run against a newer bd,
# so a mismatch here is a real incompatibility, not a style preference.
set -uo pipefail

cd "$(dirname "$0")/.."
source scripts/lockfile.sh

fail=0
check() {
  local name="$1" cmd="$2" want_key="$3" want got
  want="$(lock_version "$want_key")"
  want="${want#v}"

  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "  MISSING  $name (pinned $want) — run: make bootstrap"
    fail=1
    return
  fi
  got="$("$cmd" version 2>/dev/null || "$cmd" --version 2>/dev/null)"
  if echo "$got" | grep -qF "$want"; then
    echo "  ok       $name $want"
  else
    echo "  MISMATCH $name: pinned $want, found: $(echo "$got" | head -1)"
    fail=1
  fi
}

echo "toolchain versions (versions.lock)"
check "gt"   gt   gastown
check "bd"   bd   beads
check "dolt" dolt dolt

if [ "$fail" -ne 0 ]; then
  echo
  echo "Toolchain does not match versions.lock."
  echo "Run 'make bootstrap' and put .toolchain/bin on your PATH."
  echo "Do not upgrade gt, bd, or dolt independently: the matrix moves together."
fi
exit "$fail"
