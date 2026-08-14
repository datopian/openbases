#!/usr/bin/env python3
"""Apply and VERIFY the agent spend ceiling on each AI Gateway.

This is not in Terraform because it cannot be. The Cloudflare provider
serialises the rule field as `limit_type` while the API requires `limitType`,
so an update fails with `7001 Required`, and — more dangerously — the initial
create reports success while storing nothing. Terraform state showed a $100
limit with a server-assigned rule ID against a gateway that had none.

So this script exists, and it verifies rather than trusts: it writes the limit,
reads it back, and exits non-zero if what came back is not what was asked for.
A budget believed to be in force but absent is worse than no budget at all,
because nobody goes looking for a control they think they already have.
"""
import json
import os
import re
import sys
import urllib.error
import urllib.request

API = "https://api.cloudflare.com/client/v4"

# A sliding window. A calendar month resets on a cliff, so a spike late in one
# month and early in the next passes both checks.
WINDOW_SECONDS = 30 * 24 * 60 * 60


def call(method: str, path: str, token: str, body: dict | None = None) -> dict:
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(f"{API}{path}", data=data, method=method)
    req.add_header("Authorization", f"Bearer {token}")
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        return json.load(e)


def budget_from_tfvars(environment: str) -> float:
    """Read the agreed figure from the committed tfvars.

    Deliberately not a flag with a default: a budget passed on a command line is
    a budget that differs between whoever last ran the command, and the value in
    Git is the one that was agreed.
    """
    path = f"infra/tofu/envs/{environment}/terraform.tfvars"
    text = open(path).read()
    m = re.search(r"^\s*ai_monthly_budget\s*=\s*([0-9.]+)", text, re.M)
    if not m:
        sys.exit(f"{path} does not set ai_monthly_budget")
    return float(m.group(1))


def main() -> int:
    environment = sys.argv[1] if len(sys.argv) > 1 else "staging"

    token = os.environ.get("CLOUDFLARE_API_TOKEN")
    account = os.environ.get("CLOUDFLARE_ACCOUNT_ID")
    if not token or not account:
        sys.exit("CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID must be set")

    budget = budget_from_tfvars(environment)
    prefix = f"workgraph-{environment}-"

    listing = call("GET", f"/accounts/{account}/ai-gateway/gateways", token)
    if not listing.get("success"):
        sys.exit(f"listing gateways: {listing.get('errors')}")

    gateways = [g for g in listing["result"] if g["id"].startswith(prefix)]
    if not gateways:
        sys.exit(f"no gateways named {prefix}*; run tofu apply first")

    failures = []
    for g in gateways:
        gid = g["id"]

        # PUT is a full replace, so every managed field is resent. Omitting one
        # would silently reset it to a default — which is how a rate limit or
        # the authentication requirement could disappear while attention was on
        # the budget.
        body = {
            "id": gid,
            "cache_ttl": g.get("cache_ttl", 0),
            "cache_invalidate_on_update": g.get("cache_invalidate_on_update", False),
            "collect_logs": g.get("collect_logs", True),
            "authentication": g.get("authentication", True),
            "rate_limiting_interval": g.get("rate_limiting_interval", 60),
            "rate_limiting_limit": g.get("rate_limiting_limit", 120),
            "rate_limiting_technique": g.get("rate_limiting_technique", "sliding"),
            "spend_limits": {
                "enabled": True,
                # camelCase. The snake_case the provider sends is rejected.
                "rules": [{
                    "enabled": True,
                    "limitType": "cost",
                    "limit": budget,
                    "window": WINDOW_SECONDS,
                    "technique": "sliding",
                }],
            },
        }

        result = call("PUT", f"/accounts/{account}/ai-gateway/gateways/{gid}", token, body)
        if not result.get("success"):
            failures.append(f"{gid}: write refused: {result.get('errors')}")
            continue

        # Read back from the server rather than trusting the write response.
        # The create path proved that a success response is not evidence the
        # value was stored.
        check = call("GET", f"/accounts/{account}/ai-gateway/gateways/{gid}", token)
        limits = (check.get("result") or {}).get("spend_limits") or {}
        rules = limits.get("rules") or []

        if not limits.get("enabled") or not rules:
            failures.append(f"{gid}: no spend limit after writing one")
            continue
        actual = rules[0].get("limit")
        if float(actual) != budget:
            failures.append(f"{gid}: limit is {actual}, expected {budget}")
            continue
        if not g.get("authentication"):
            failures.append(f"{gid}: authentication is off; the URL alone could spend")
            continue

        print(f"  {gid}: ${actual:g} per 30 days, verified")

    if failures:
        print("\nspend limit verification FAILED:", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1

    print(f"all {len(gateways)} gateway(s) carry the ${budget:g} ceiling")
    return 0


if __name__ == "__main__":
    sys.exit(main())
