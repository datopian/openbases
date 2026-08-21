#!/usr/bin/env bash
# WP-I3: the 24-hour soak. Run ON the control node, detached.
#
#   systemd-run --unit=wg-soak --collect \
#     --setenv=HOURS=24 /usr/local/bin/wg-soak
#
# What a soak finds that no other test does: things that only appear over time.
# Memory creeping up, database connections not being returned, disk filling with
# logs and Dolt history, a fifteen-minute timer gradually falling behind until
# runs overlap. Staging has never run more than a few hours of real activity, so
# a leak that takes six hours to matter has been invisible in every test.
#
# SCOPE, and this is a deliberate choice about money. The load target names "25
# active agents", and twenty-five real agents for twenty-four hours is genuine
# spend through the gateway for very little extra information — idle inference
# proves nothing that a bounded window does not. So this soaks the control plane:
# webhook ingestion, projections, queries, the reconcile timer. The agent path
# is proven separately by the resource-limit test and a dispatch, both bounded.
#
# Run as a transient systemd unit, not under an SSH session: the Access session
# expires long before 24 hours are up, and a soak that dies with the connection
# measures the connection.
set -uo pipefail

HOURS="${HOURS:-24}"
EVERY="${EVERY:-300}"           # seconds between samples
EVENTS="${EVENTS:-10}"          # webhooks per sample
OUT="${OUT:-/var/log/wg-soak.jsonl}"
URL="http://127.0.0.1:8080/v1/integrations/github/webhook"
SECRET_FILE="/etc/workgraph/credentials/github_webhook_secret"

[ -r "$SECRET_FILE" ] || { echo "cannot read $SECRET_FILE" >&2; exit 1; }
SECRET=$(cat "$SECRET_FILE")

q() { su - postgres -c "psql -At -d workgraph -c \"$1\"" 2>/dev/null | tr -d '[:space:]'; }

deadline=$(( $(date +%s) + HOURS * 3600 ))
sample=0
echo "soak starting: ${HOURS}h, a sample every ${EVERY}s, ${EVENTS} events per sample -> $OUT"

while [ "$(date +%s)" -lt "$deadline" ]; do
  sample=$((sample + 1))
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)

  # --- send some traffic -------------------------------------------------
  sent=0; failed=0; worst=0
  for i in $(seq 1 "$EVENTS"); do
    body="{\"action\":\"completed\",\"check_run\":{\"id\":${sample}${i},\"name\":\"soak\",\"conclusion\":\"success\"},\"repository\":{\"full_name\":\"datopian/workgraph-agent-sandbox\"}}"
    sig="sha256=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $2}')"
    t0=$(date +%s%N)
    code=$(curl -s -o /dev/null -w '%{http_code}' -m 15 -X POST "$URL" \
      -H 'Content-Type: application/json' -H 'X-GitHub-Event: check_run' \
      -H "X-GitHub-Delivery: soak-${sample}-${i}-$(date +%s)" \
      -H "X-Hub-Signature-256: ${sig}" --data-binary "$body")
    ms=$(( ( $(date +%s%N) - t0 ) / 1000000 ))
    [ "$ms" -gt "$worst" ] && worst=$ms
    case "$code" in 2*) sent=$((sent+1)) ;; *) failed=$((failed+1)) ;; esac
  done

  # --- read a representative query ---------------------------------------
  t0=$(date +%s%N)
  projects=$(q "SELECT count(*) FROM projects")
  query_ms=$(( ( $(date +%s%N) - t0 ) / 1000000 ))

  # --- the things that drift ---------------------------------------------
  pid=$(systemctl show -p MainPID --value control-api)
  rss_kb=$(awk '/^VmRSS/{print $2}' "/proc/${pid}/status" 2>/dev/null)
  fds=$(ls "/proc/${pid}/fd" 2>/dev/null | wc -l | tr -d ' ')
  conns=$(q "SELECT count(*) FROM pg_stat_activity WHERE datname='workgraph'")
  backlog=$(q "SELECT count(*) FROM github_deliveries WHERE processed_at IS NULL")
  deliveries=$(q "SELECT count(*) FROM github_deliveries")
  db_bytes=$(q "SELECT pg_database_size('workgraph')")
  disk_pct=$(df --output=pcent / | tail -1 | tr -dc '0-9')
  load=$(awk '{print $1}' /proc/loadavg)
  api_up=$(systemctl is-active control-api)
  recon_result=$(systemctl show -p Result --value workgraph-reconcile.service 2>/dev/null)
  journal_mb=$(du -sm /var/log/journal 2>/dev/null | awk '{print $1}')

  printf '{"at":"%s","sample":%d,"sent":%d,"failed":%d,"worst_ms":%d,"query_ms":%d,' \
    "$now" "$sample" "$sent" "$failed" "$worst" "$query_ms" >> "$OUT"
  printf '"rss_kb":%s,"fds":%s,"pg_conns":%s,"backlog":%s,"deliveries":%s,' \
    "${rss_kb:-0}" "${fds:-0}" "${conns:-0}" "${backlog:-0}" "${deliveries:-0}" >> "$OUT"
  printf '"db_bytes":%s,"disk_pct":%s,"load":%s,"journal_mb":%s,"api":"%s","reconcile":"%s","projects":%s}\n' \
    "${db_bytes:-0}" "${disk_pct:-0}" "${load:-0}" "${journal_mb:-0}" "${api_up}" "${recon_result:-unknown}" "${projects:-0}" >> "$OUT"

  sleep "$EVERY"
done

echo "soak complete after ${sample} samples"
