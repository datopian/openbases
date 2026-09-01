#!/usr/bin/env bash
# Prove check_infra.py's Access bypass guard fails when it should.
#
# A guard that cannot fail is worse than no guard, because it is read as
# evidence. Each case below is a way a bypass could reach production, and the
# last case asserts the unmodified tree still passes -- without it, a check that
# fails on everything would look like a success here.
#
# Restores by copy, never by `git checkout`: the guard was once wiped mid-proof
# by a checkout of its own uncommitted source, and the run then reported OK
# because there was no longer a check to fail.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
MODULE=infra/tofu/modules/environment/main.tf
SCRIPT=scripts/check_infra.py

WORK=$(mktemp -d)
cp "$MODULE" "$WORK/main.tf"
cp "$SCRIPT" "$WORK/check_infra.py"
restore() { cp "$WORK/main.tf" "$MODULE"; cp "$WORK/check_infra.py" "$SCRIPT"; }
trap 'restore; rm -rf "$WORK"' EXIT

fails=0
expect_fail() {
  local label="$1" want="$2" out
  # Captured, not piped. Under `set -o pipefail` a pipeline takes the failing
  # exit status, and check_infra.py exits 1 precisely when the case works -- so
  # `python3 ... | grep -q` reports every success as a failure.
  out=$(python3 "$SCRIPT" 2>&1 || true)
  if printf '%s' "$out" | grep -q -- "$want"; then
    printf '  PASS  %s\n' "$label"
  else
    printf '  FAIL  %s (expected a complaint matching: %s)\n' "$label" "$want"
    fails=$((fails + 1))
  fi
  restore
}

echo "Access bypass guard:"

cat >> "$MODULE" <<'EOF'

resource "cloudflare_zero_trust_access_policy" "sneaky_bypass" {
  account_id = var.cloudflare_account_id
  decision   = "bypass"
}
EOF
expect_fail "a fourth, undeclared bypass is refused" "undeclared Access bypass policies"

# The lazy-regex trap: `decision` after a nested block. A `.*?\n\}` body match
# stops at the first nested close and never sees it.
cat >> "$MODULE" <<'EOF'

resource "cloudflare_zero_trust_access_policy" "nested_bypass" {
  account_id = var.cloudflare_account_id
  include = [
    {
      everyone = {}
    }
  ]
  decision = "bypass"
}
EOF
expect_fail "a bypass declared after a nested block is still seen" "nested_bypass"

cat >> "$MODULE" <<'EOF'

resource "cloudflare_zero_trust_access_policy" "webhook_bypass_extra" {
  account_id = var.cloudflare_account_id
  decision   = "bypass"
}
resource "cloudflare_zero_trust_access_application" "everything" {
  account_id = var.cloudflare_account_id
  domain     = "*.openbases.com"
  policies = [
    { id = cloudflare_zero_trust_access_policy.webhook_bypass_extra.id, precedence = 1 }
  ]
}
EOF
expect_fail "a bypass on a wildcard domain is refused" "wildcard domain"

cat >> "$MODULE" <<'EOF'

resource "cloudflare_zero_trust_access_policy" "hostwide_bypass" {
  account_id = var.cloudflare_account_id
  decision   = "bypass"
}
resource "cloudflare_zero_trust_access_application" "hostwide" {
  account_id = var.cloudflare_account_id
  domain     = "work-staging.openbases.com"
  policies = [
    { id = cloudflare_zero_trust_access_policy.hostwide_bypass.id, precedence = 1 }
  ]
}
EOF
expect_fail "a bypass over a whole host rather than a path is refused" "whole host"

python3 - <<'PY'
import pathlib
p = pathlib.Path("scripts/check_infra.py")
p.write_text(p.read_text().replace('"api_token_bypass"}', '"api_token_bypass", "gone_last_year"}', 1))
PY
expect_fail "a stale name left in the allow-list is refused" "do not exist"

# The control. Without it, a check that failed on everything would score 5/5.
if python3 "$SCRIPT" >/dev/null 2>&1; then
  printf '  PASS  the unmodified tree still passes\n'
else
  printf '  FAIL  the unmodified tree does not pass\n'
  python3 "$SCRIPT" 2>&1 | head -5
  fails=$((fails + 1))
fi

echo
if [ "$fails" -ne 0 ]; then
  echo "$fails case(s) did not behave as required"
  exit 1
fi
echo "the bypass guard fails on every way in, and passes on the real tree"
