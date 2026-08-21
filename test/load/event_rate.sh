#!/usr/bin/env bash
# WP-I3 criterion 2: event freshness at 100 GitHub events a minute.
#
# Run ON the control node. Posts signed webhook deliveries at a target rate to
# the loopback listener and measures whether ingestion keeps up.
#
# Why this matters more than it sounds: the portfolio view's whole value is that
# it is current. If ingestion falls behind, nobody gets an error — a project just
# shows a pull request as open that merged an hour ago. A view that is silently
# stale is worse than one that is visibly down, because people keep trusting it.
#
# Signed properly, with the real secret from the systemd credential, so this
# exercises the actual path: HMAC verification, delivery deduplication, and the
# insert into github_deliveries. An unsigned flood would measure a 401 handler.
set -uo pipefail

RATE="${RATE:-100}"          # events per minute, the WP-I3 target
MINUTES="${MINUTES:-2}"
URL="${URL:-http://127.0.0.1:8080/v1/integrations/github/webhook}"

# The secret the running service is actually using, not a copy that might have
# drifted. systemd puts it in a per-service tmpfs.
SECRET_FILE="/etc/workgraph/credentials/github_webhook_secret"
[ -r "$SECRET_FILE" ] || { echo "cannot read $SECRET_FILE (run as root)" >&2; exit 1; }
SECRET=$(cat "$SECRET_FILE")

total=$(( RATE * MINUTES ))
interval=$(awk -v r="$RATE" 'BEGIN {printf "%.3f", 60/r}')
echo "Event ingestion: ${RATE}/min for ${MINUTES} min (${total} deliveries, one every ${interval}s)"

pass() { printf '  PASS  %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures+1)); }
failures=0

count_rows() { su - postgres -c "psql -At -d workgraph -c 'SELECT count(*) FROM github_deliveries'" 2>/dev/null | tr -d '[:space:]'; }
count_unprocessed() { su - postgres -c "psql -At -d workgraph -c 'SELECT count(*) FROM github_deliveries WHERE processed_at IS NULL'" 2>/dev/null | tr -d '[:space:]'; }

before=$(count_rows)
echo "  deliveries already stored: ${before:-?}"

accepted=0; rejected=0; slow=0
worst=0
start=$(date +%s)

for i in $(seq 1 "$total"); do
  # A plausible check_run payload. The shape matters only in so far as the
  # handler stores it; the cost being measured is signature verification plus
  # the insert.
  body="{\"action\":\"completed\",\"check_run\":{\"id\":${i},\"name\":\"ci\",\"conclusion\":\"success\",\"head_sha\":\"$(printf '%040d' "$i")\"},\"repository\":{\"full_name\":\"datopian/workgraph-agent-sandbox\"}}"
  sig="sha256=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $2}')"

  t0=$(date +%s%N)
  code=$(curl -s -o /dev/null -w '%{http_code}' -m 10 -X POST "$URL" \
    -H "Content-Type: application/json" \
    -H "X-GitHub-Event: check_run" \
    -H "X-GitHub-Delivery: load-$(date +%s)-${i}" \
    -H "X-Hub-Signature-256: ${sig}" \
    --data-binary "$body")
  ms=$(( ( $(date +%s%N) - t0 ) / 1000000 ))
  [ "$ms" -gt "$worst" ] && worst=$ms
  # 500 ms is the point at which a burst starts to queue behind itself at this
  # rate, since the interval is 600 ms.
  [ "$ms" -gt 500 ] && slow=$((slow+1))

  case "$code" in
    2*) accepted=$((accepted+1)) ;;
    *)  rejected=$((rejected+1)); [ "$rejected" -le 3 ] && echo "  .. HTTP $code on delivery $i" ;;
  esac

  # Pace to the target rate rather than flooding: the question is whether the
  # system keeps up with GitHub's rate, not what its absolute ceiling is.
  sleep "$interval"
done

elapsed=$(( $(date +%s) - start ))
after=$(count_rows)
stored=$(( ${after:-0} - ${before:-0} ))
unprocessed=$(count_unprocessed)

echo
echo "  sent ${total} in ${elapsed}s; accepted ${accepted}, rejected ${rejected}"
echo "  worst single delivery ${worst} ms; ${slow} over 500 ms"
echo "  rows stored ${stored}; unprocessed backlog ${unprocessed:-?}"

[ "$rejected" -eq 0 ] && pass "every delivery was accepted" \
  || fail "${rejected} deliveries rejected"

[ "$stored" -ge "$accepted" ] && pass "every accepted delivery reached the database (${stored})" \
  || fail "accepted ${accepted} but only ${stored} rows appeared"

# The rate is sustained if the run took roughly the wall time the target implies.
budget=$(( MINUTES * 60 * 130 / 100 ))
[ "$elapsed" -le "$budget" ] && pass "kept up with the target rate (${elapsed}s against a ${budget}s budget)" \
  || fail "fell behind: ${elapsed}s for what should take $(( MINUTES * 60 ))s"

[ "$worst" -le 2000 ] && pass "no delivery took longer than 2s (worst ${worst} ms)" \
  || fail "a delivery took ${worst} ms; GitHub times out and retries at 10s"

echo
if [ "$failures" -eq 0 ]; then echo "Event ingestion: all checks passed."
else echo "Event ingestion: ${failures} check(s) FAILED."; fi
exit "$failures"
