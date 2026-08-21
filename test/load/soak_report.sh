#!/usr/bin/env bash
# Summarise a soak run: what drifted, and did anything break.
#
# The question a soak answers is not "was it up" but "did anything grow without
# bound". A flat line is the result you want and the one that is easiest to
# skim past, so this reports first and last values side by side.
set -uo pipefail
OUT="${1:-/var/log/wg-soak.jsonl}"
[ -r "$OUT" ] || { echo "no soak log at $OUT" >&2; exit 1; }

python3 - "$OUT" <<'PY'
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
if not rows:
    sys.exit("soak log is empty")
first, last = rows[0], rows[-1]
hours = len(rows) * 5 / 60

print(f"{len(rows)} samples, about {hours:.1f} hours")
print(f"  from {first['at']} to {last['at']}")
print()

def drift(key, label, unit="", scale=1):
    a, b = first.get(key, 0)/scale, last.get(key, 0)/scale
    pct = ((b - a) / a * 100) if a else 0
    flag = "  <-- grew" if pct > 50 else ""
    print(f"  {label:24s} {a:>12.1f} -> {b:>12.1f} {unit:<6s} {pct:+7.1f}%{flag}")

print("what drifted:")
drift("rss_kb", "control-api RSS", "MiB", 1024)
drift("fds", "open file descriptors")
drift("pg_conns", "PostgreSQL connections")
drift("db_bytes", "database size", "MiB", 1024*1024)
drift("journal_mb", "journal", "MiB")
drift("disk_pct", "disk used", "%")
print()

sent = sum(r.get("sent", 0) for r in rows)
failed = sum(r.get("failed", 0) for r in rows)
worst = max((r.get("worst_ms", 0) for r in rows), default=0)
qworst = max((r.get("query_ms", 0) for r in rows), default=0)
backlog_max = max((r.get("backlog", 0) for r in rows), default=0)
down = [r["at"] for r in rows if r.get("api") != "active"]
recon_bad = sorted({r.get("reconcile") for r in rows if r.get("reconcile") not in ("success", "", None)})

print("what happened:")
print(f"  webhooks sent            {sent}, failed {failed}")
print(f"  worst webhook            {worst} ms")
print(f"  worst query              {qworst} ms")
print(f"  peak delivery backlog    {backlog_max}")
print(f"  samples with API down    {len(down)}")
print(f"  reconcile non-success    {recon_bad or 'none'}")
print()

problems = []
if failed: problems.append(f"{failed} webhook deliveries failed")
if down: problems.append(f"the API was not active in {len(down)} sample(s)")
if recon_bad: problems.append(f"reconcile reported {recon_bad}")
if first.get("rss_kb") and last["rss_kb"] > first["rss_kb"] * 2:
    problems.append("control-api RSS more than doubled — a leak, not variance")
if first.get("pg_conns") and last["pg_conns"] > first["pg_conns"] * 3:
    problems.append("PostgreSQL connections tripled — the pool is not returning them")
if last.get("disk_pct", 0) > 85: problems.append(f"disk at {last['disk_pct']}%")
if backlog_max > 200: problems.append(f"delivery backlog peaked at {backlog_max} — projections fell behind")

if problems:
    print("PROBLEMS:")
    for p in problems: print(f"  - {p}")
    sys.exit(1)
print("nothing grew without bound, nothing failed.")
PY
