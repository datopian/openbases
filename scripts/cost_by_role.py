#!/usr/bin/env python3
"""Read the AI Gateway bill by role, not just by model.

The first real bill was $23 and could not be acted on. AI Gateway logs the
model, several roles share a model, and the interesting question — what did the
supervision cost compared with the work — had no answer in the data. It had to
be inferred from request counts and timing, which is how "the patrol roles cost
more than the work" started as a guess.

Every agent role now tags its requests with cf-aig-metadata (role, cell, rig),
written into each role's settings.json by the gastown Ansible role. This reads
those logs back and groups by the tag.

Usage:
  scripts/cost_by_role.py [--gateway workgraph-staging-oss] [--hours 24]

Needs CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID:
  set -a; . ~/.config/datopian-workgraph/credentials.env; set +a
"""
import argparse
import collections
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone

API = "https://api.cloudflare.com/client/v4"


def fetch(account: str, token: str, gateway: str, since: datetime, cap: int):
    """Page through the gateway log, newest first, stopping at `since`.

    Paged rather than taking the first response, because a busy day easily
    exceeds one page and a truncated read would quietly under-report the very
    roles the report exists to find.
    """
    # 50 is the API maximum; asking for more is a 400, not a clamp.
    rows, page, per_page = [], 1, 50
    while len(rows) < cap:
        query = urllib.parse.urlencode({
            "per_page": per_page,
            "page": page,
            "order_by": "created_at",
            "order_by_direction": "desc",
        })
        url = f"{API}/accounts/{account}/ai-gateway/gateways/{gateway}/logs?{query}"
        req = urllib.request.Request(url)
        req.add_header("Authorization", f"Bearer {token}")
        try:
            with urllib.request.urlopen(req, timeout=60) as r:
                body = json.load(r)
        except urllib.error.HTTPError as e:
            detail = e.read().decode("utf-8", "replace")[:200]
            print(f"gateway {gateway}: HTTP {e.code} {detail}", file=sys.stderr)
            return rows, False

        if not body.get("success"):
            print(f"gateway {gateway}: {json.dumps(body.get('errors'))[:200]}", file=sys.stderr)
            return rows, False

        batch = body.get("result") or []
        if not batch:
            return rows, True

        for row in batch:
            stamp = (row.get("created_at") or "")[:19]
            try:
                when = datetime.fromisoformat(stamp).replace(tzinfo=timezone.utc)
            except ValueError:
                continue
            if when < since:
                return rows, True
            rows.append(row)
        page += 1
    # Hit the cap with more to read. Said out loud rather than silently
    # truncated, because a partial total that looks complete is worse than none.
    return rows, False


def metadata_of(row) -> dict:
    md = row.get("metadata") or {}
    if isinstance(md, str):
        try:
            md = json.loads(md)
        except ValueError:
            return {}
    return md if isinstance(md, dict) else {}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--gateway", action="append", default=None,
                    help="gateway id; repeatable. Defaults to the three staging gateways.")
    ap.add_argument("--hours", type=float, default=24.0)
    ap.add_argument("--max-rows", type=int, default=5000)
    args = ap.parse_args()

    token = os.environ.get("CLOUDFLARE_API_TOKEN")
    account = os.environ.get("CLOUDFLARE_ACCOUNT_ID")
    if not token or not account:
        print("set CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID", file=sys.stderr)
        return 2

    gateways = args.gateway or [
        "workgraph-staging-oss",
        "workgraph-staging-internal",
        "workgraph-staging-client",
    ]
    since = datetime.now(timezone.utc) - timedelta(hours=args.hours)

    reqs = collections.Counter()
    cost = collections.Counter()
    out_tokens = collections.Counter()
    complete = True

    for gateway in gateways:
        rows, whole = fetch(account, token, gateway, since, args.max_rows)
        complete = complete and whole
        for row in rows:
            md = metadata_of(row)
            # Untagged is a category, not a rounding error: it is how a role
            # that never got its settings written shows up.
            role = md.get("role") or "(untagged)"
            rig = md.get("rig")
            label = f"{role}@{rig}" if rig else role
            key = (gateway.replace("workgraph-staging-", ""), label, row.get("model") or "?")
            reqs[key] += 1
            cost[key] += float(row.get("cost") or 0)
            out_tokens[key] += int(row.get("tokens_out") or 0)

    if not reqs:
        print(f"no requests in the last {args.hours:g}h")
        return 0

    print(f"{'cell':9s} {'role':16s} {'model':34s} {'req':>5s} {'out tok':>9s} {'cost':>9s}")
    for key, n in sorted(cost.items(), key=lambda kv: -kv[1]):
        cellname, label, model = key
        print(f"{cellname:9s} {label:16s} {model:34s} {n if False else reqs[key]:5d} "
              f"{out_tokens[key]:9d} {cost[key]:9.4f}")

    total = sum(cost.values())
    print(f"\ntotal {total:.4f} USD across {sum(reqs.values())} requests "
          f"in the last {args.hours:g}h")

    by_role = collections.Counter()
    for (_cell, label, _model), value in cost.items():
        by_role[label] += value
    print("\nby role:")
    for label, value in by_role.most_common():
        share = (value / total * 100) if total else 0
        print(f"  {label:16s} {value:9.4f}  {share:5.1f}%")

    if not complete:
        print("\nWARNING: the window was not read to the end; totals are a lower bound.",
              file=sys.stderr)
        return 1

    # The gateway's own cost figure is an estimate and is visibly odd on very
    # small requests — a two-token prompt should not cost twenty cents. Treat
    # the SHARES as the answer and the absolute numbers as indicative.
    return 0


if __name__ == "__main__":
    sys.exit(main())
