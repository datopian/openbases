#!/usr/bin/env bash
# Drill: running out of money must not produce a retry storm (wg-01h).
#
# This is a DRILL, not a test. It is run deliberately, by a person, on staging,
# and it deliberately breaks the budget for a few minutes. It is not in CI and
# must never be.
#
# Why it needs to exist at all. When the pool is exhausted the gateway returns
# 429 without forwarding, so the rejected requests themselves are free. The risk
# is not the rejections — it is what the stack does about them:
#
#   Claude Code retries a 429 with its own backoff
#   a failed agent may exit, and the Gas Town daemon restarts the session
#   every restart re-primes context, which is input tokens spent to fail again
#
# That last loop is the expensive one, because it costs on every attempt while
# no attempt can succeed. Nobody has watched it happen, and "the gateway does
# not retry" — which is true, retry_max_attempts is null — only rules out the
# cheapest of the three.
#
# Method: move the ceiling BELOW what the window has already spent, so the next
# request is refused without spending anything to arrange it. Then run the town
# and count.
#
# Run from a workstation with the Cloudflare credentials sourced:
#   set -a; . ~/.config/datopian-workgraph/credentials.env; set +a
#   test/drills/budget_exhaustion.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

GATEWAY="${WG_DRILL_GATEWAY:-workgraph-staging-oss}"
# The smallest share is 10%, and the API rejects a limit that rounds to zero,
# so the pool floor is $0.10. That puts oss at $0.07 — about 40 calls at the
# larger max_tokens the spend loop uses.
DRILL_POOL="${WG_DRILL_POOL:-0.10}"
WATCH_SECONDS="${WG_DRILL_WATCH:-300}"
EXEC_HOST="${WG_EXEC_HOST:-ssh-exec-staging.openbases.com}"
SSH=(ssh -o ProxyCommand="cloudflared access ssh --hostname %h"
     -o StrictHostKeyChecking=no -o ConnectTimeout=30 "root@${EXEC_HOST}")

: "${CLOUDFLARE_API_TOKEN:?source ~/.config/datopian-workgraph/credentials.env}"
: "${CLOUDFLARE_ACCOUNT_ID:?}"
: "${WG_AI_GATEWAY_TOKEN:?}"

say() { printf '\n== %s\n' "$1"; }

# Restoring the budget is the one thing that MUST happen, including on Ctrl-C
# and including if a step above it fails. A drill that leaves the ceiling at a
# dollar is worse than never running it.
restore() {
  say "restoring the agreed budget"
  python3 scripts/ai_gateway_spend_limits.py staging || {
    echo "!! THE BUDGET DID NOT RESTORE. Run this by hand, now:" >&2
    echo "   python3 scripts/ai_gateway_spend_limits.py staging" >&2
    return 1
  }
}
teardown() {
  "${SSH[@]}" 'su -s /bin/bash -c "export PATH=/usr/local/bin:\$PATH HOME=/srv/cells/oss
cd /srv/cells/oss/town && gt down >/dev/null 2>&1; gt daemon stop >/dev/null 2>&1" wgcell_oss
    sleep 3; pkill -u wgcell_oss -f claude 2>/dev/null; true' >/dev/null 2>&1
  restore
}
trap teardown EXIT INT TERM

probe() {
  /usr/bin/curl -s -o /tmp/wg-drill-body.json -w '%{http_code}' -X POST \
    "https://gateway.ai.cloudflare.com/v1/${CLOUDFLARE_ACCOUNT_ID}/${GATEWAY}/anthropic/v1/messages" \
    -H "cf-aig-authorization: Bearer ${WG_AI_GATEWAY_TOKEN}" \
    -H "anthropic-version: 2023-06-01" -H "Content-Type: application/json" \
    -d '{"model":"claude-haiku-4-5-20251001","max_tokens":900,
         "messages":[{"role":"user","content":"Explain HTTP caching in detail."}]}'
}

say "1. baseline: the gateway currently accepts a request"
before=$(probe)
echo "   HTTP $before"
if [ "$before" != "200" ]; then
  echo "   the gateway is not healthy before the drill; stopping" >&2
  exit 1
fi

say "2. moving the ceiling below what this window has already spent"
python3 scripts/ai_gateway_spend_limits.py staging "--pool=${DRILL_POOL}" || exit 1
sleep 10

say "3. spending up to the ceiling, then confirming the gateway refuses"
# The counter starts from zero whenever the rule is written — writing a rule
# assigns a new rule id and the spend counter is keyed to it (wg-18a). So the
# drill cannot rely on the window's history; it has to spend the ceiling itself.
# Haiku is about $0.00052 a call, so a $0.01 ceiling falls after roughly 20.
during=""
for i in $(seq 1 "${WG_DRILL_SPEND_MAX:-120}"); do
  during=$(probe)
  if [ "$during" != "200" ]; then
    echo "   refused at request #$i with HTTP $during"
    head -c 220 /tmp/wg-drill-body.json; echo
    break
  fi
  sleep 1
done
if [ "$during" = "200" ]; then
  echo "   the gateway still accepts requests after ${WG_DRILL_SPEND_MAX:-60} of them;" >&2
  echo "   the ceiling is too high for this drill, or the limit did not apply." >&2
  exit 1
fi

say "4. running the town against an exhausted budget for ${WATCH_SECONDS}s"
start_iso=$(date -u +%Y-%m-%dT%H:%M:%SZ)
"${SSH[@]}" 'su -s /bin/bash -c "export PATH=/usr/local/bin:\$PATH HOME=/srv/cells/oss
cd /srv/cells/oss/town && gt dolt start >/dev/null 2>&1" wgcell_oss; sleep 10
  su -s /bin/bash -c "export PATH=/usr/local/bin:\$PATH HOME=/srv/cells/oss
cd /srv/cells/oss/town && gt daemon start >/dev/null 2>&1" wgcell_oss' >/dev/null 2>&1

# Sampled rather than waited out in one block, so a runaway is visible while it
# is happening rather than only in the totals.
elapsed=0
while [ "$elapsed" -lt "$WATCH_SECONDS" ]; do
  sleep 60
  elapsed=$((elapsed + 60))
  procs=$("${SSH[@]}" 'pgrep -u wgcell_oss -c -f claude 2>/dev/null || echo 0' 2>/dev/null | tr -d '[:space:]')
  printf '   t+%3ds  agent processes: %s\n' "$elapsed" "${procs:-?}"
done

say "5. what the daemon did about it"
"${SSH[@]}" 'su -s /bin/bash -c "export PATH=/usr/local/bin:\$PATH HOME=/srv/cells/oss
cd /srv/cells/oss/town && gt daemon logs 2>/dev/null | tail -400" wgcell_oss' 2>/dev/null \
  | grep -icE "restart|respawn|crash loop|backoff" \
  | sed 's/^/   daemon lines mentioning restart, respawn, crash loop or backoff: /'

say "6. request volume while the budget was exhausted"
WG_DRILL_SINCE="$start_iso" GATEWAY="$GATEWAY" python3 - <<'PY'
import json, os, urllib.request, collections
from datetime import datetime, timezone

acc = os.environ["CLOUDFLARE_ACCOUNT_ID"]; tok = os.environ["CLOUDFLARE_API_TOKEN"]
gw = os.environ["GATEWAY"]
since = datetime.fromisoformat(os.environ["WG_DRILL_SINCE"].replace("Z", "+00:00"))

rows, page = [], 1
while page <= 20:
    url = (f"https://api.cloudflare.com/client/v4/accounts/{acc}/ai-gateway/gateways/{gw}"
           f"/logs?per_page=50&page={page}&order_by=created_at&order_by_direction=desc")
    req = urllib.request.Request(url); req.add_header("Authorization", f"Bearer {tok}")
    with urllib.request.urlopen(req, timeout=60) as r:
        batch = (json.load(r).get("result") or [])
    if not batch:
        break
    stop = False
    for row in batch:
        try:
            when = datetime.fromisoformat((row.get("created_at") or "")[:19]).replace(tzinfo=timezone.utc)
        except ValueError:
            continue
        if when < since:
            stop = True
            break
        rows.append(row)
    if stop:
        break
    page += 1

by_status = collections.Counter(str(r.get("status_code")) for r in rows)
by_role = collections.Counter()
for r in rows:
    md = r.get("metadata") or {}
    if isinstance(md, str):
        try: md = json.loads(md)
        except ValueError: md = {}
    by_role[md.get("role", "(untagged)")] += 1

mins = max((datetime.now(timezone.utc) - since).total_seconds() / 60, 1)
print(f"   {len(rows)} requests in {mins:.1f} min = {len(rows)/mins:.1f}/min")
print(f"   by status: {dict(by_status)}")
print(f"   by role:   {dict(by_role)}")
spent = sum(float(r.get('cost') or 0) for r in rows)
# The number that decides it. Rejected requests are free; a storm is only
# expensive if something keeps paying to be rejected.
print(f"   cost incurred while exhausted: ${spent:.4f}")
PY

say "drill complete; the trap restores the budget next"
