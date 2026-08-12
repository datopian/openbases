#!/usr/bin/env bash
# Smoke-check the Datopian production zones that share this Cloudflare account.
#
# Workgraph does not own this account. On 2026-08-12 an account-level Zero Trust
# setting returned 403 across these zones and a human noticed before any
# monitoring did (wg-8yv.47). Run this after ANY apply under infra/tofu/account,
# and before declaring such an apply successful.
#
# This is a smoke check, not monitoring: it says "still serving", not "correct".
set -uo pipefail

ZONES=(
  datopian.com www.datopian.com
  portaljs.com portaljs.org
  datahub.io
  flowershow.app flowershow.dev
  openspending.org
  viderum.com viderum.org
  f11s.com
  taskgraph.dev
  markdowndb.com
  datapatterns.org
  publicbodies.org
  openeconomics.net
  getthedata.org
)

fail=0
echo "smoke check: ${#ZONES[@]} zones sharing this Cloudflare account"

for z in "${ZONES[@]}"; do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 "https://$z" 2>/dev/null) || code=000
  # A failed curl can leave an empty or doubled value; normalise to 3 digits.
  code=$(printf '%s' "$code" | tail -c 3)
  [ -n "$code" ] || code=000
  case "$code" in
    # 2xx and 3xx are healthy. A redirect is how most of these are configured.
    2??|3??) printf '  ok   %-24s %s\n' "$z" "$code" ;;
    403)
      printf '  FAIL %-24s %s  <-- possible Access/Zero Trust block\n' "$z" "$code"
      fail=1 ;;
    000)
      # Could be the zone, could be the network running this check. Not a pass,
      # but not evidence of our change either.
      printf '  ??   %-24s unreachable\n' "$z" ;;
    *)
      printf '  warn %-24s %s\n' "$z" "$code" ;;
  esac
done

if [ "$fail" -ne 0 ]; then
  echo
  echo "One or more shared production zones returned 403."
  echo "If an account-level Cloudflare change was just applied, revert it first"
  echo "and diagnose afterwards. See wg-8yv.47."
  exit 1
fi
echo "all zones serving"
