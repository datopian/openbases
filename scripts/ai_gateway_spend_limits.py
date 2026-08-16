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

It also DIVIDES the pool rather than repeating it. The agreed figure is a single
shared ceiling across security domains, and writing the whole of it to each of
the three gateways made the real ceiling three times that — a budget believed to
be $100 that was in fact $300, which is the same failure in a different costume.

Cloudflare cannot express a limit spanning gateways and exposes no spend
endpoint to sum them with, so the enforcement is arithmetic: three limits whose
shares add to 1 cannot together exceed the pool, and Cloudflare's own accounting
does the rest with none of our code in the path.

The cost of that is real and worth stating: a domain can be refused while the
pool still has room. Moving headroom between domains automatically is wg-o7t,
and it is blocked on a trustworthy spend figure — the per-request cost in the
logs is visibly wrong on small requests.
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


def budget_from_tfvars(environment: str) -> tuple[float, dict[str, float]]:
    """Read the agreed pool and its division from the committed tfvars.

    Deliberately not flags with defaults: a budget passed on a command line is a
    budget that differs between whoever last ran the command, and the value in
    Git is the one that was agreed.

    Returns (pool, shares). The shares must sum to 1, because the whole point is
    that the per-gateway limits add up to the pool — Cloudflare cannot express a
    limit spanning gateways, so the sum IS the enforcement.
    """
    path = f"infra/tofu/envs/{environment}/terraform.tfvars"
    text = open(path).read()

    m = re.search(r"^\s*ai_monthly_budget\s*=\s*([0-9.]+)", text, re.M)
    if not m:
        sys.exit(f"{path} does not set ai_monthly_budget")
    pool = float(m.group(1))

    block = re.search(r"^\s*ai_budget_shares\s*=\s*\{(.*?)\}", text, re.M | re.S)
    if not block:
        sys.exit(f"{path} does not set ai_budget_shares; the pool cannot be divided")
    shares = {
        k: float(v)
        for k, v in re.findall(r"([a-z_]+)\s*=\s*([0-9.]+)", block.group(1))
    }
    if not shares:
        sys.exit(f"{path}: ai_budget_shares is empty")

    total = sum(shares.values())
    # Checked rather than normalised. Shares that do not sum to 1 mean somebody
    # edited one and not the others, and silently rescaling them would apply a
    # budget nobody wrote down — which is the exact failure this script exists
    # to prevent.
    if abs(total - 1.0) > 1e-6:
        sys.exit(f"{path}: ai_budget_shares sum to {total:g}, not 1. "
                 "The per-gateway limits must add up to the pool.")
    return pool, shares


def main() -> int:
    environment = sys.argv[1] if len(sys.argv) > 1 else "staging"

    token = os.environ.get("CLOUDFLARE_API_TOKEN")
    account = os.environ.get("CLOUDFLARE_ACCOUNT_ID")
    if not token or not account:
        sys.exit("CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID must be set")

    pool, shares = budget_from_tfvars(environment)
    prefix = f"workgraph-{environment}-"

    listing = call("GET", f"/accounts/{account}/ai-gateway/gateways", token)
    if not listing.get("success"):
        sys.exit(f"listing gateways: {listing.get('errors')}")

    gateways = [g for g in listing["result"] if g["id"].startswith(prefix)]
    if not gateways:
        sys.exit(f"no gateways named {prefix}*; run tofu apply first")

    # Every gateway must have a share, and every share a gateway. A domain with
    # no share would be written a limit of zero and refuse everything; a share
    # with no gateway means part of the pool is allocated to nothing, and the
    # sum no longer describes the real ceiling.
    found = {g["id"][len(prefix):] for g in gateways}
    if found != set(shares):
        sys.exit(f"gateways {sorted(found)} do not match shares {sorted(shares)}")

    failures = []
    for g in gateways:
        gid = g["id"]
        domain = gid[len(prefix):]
        budget = round(pool * shares[domain], 2)

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

        print(f"  {gid}: ${actual:g} per 30 days "
              f"({shares[domain]:.0%} of the ${pool:g} pool), verified")

    if failures:
        print("\nspend limit verification FAILED:", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1

    # The sum, not the last loop variable. The previous wording reported
    # "all 3 gateways carry the $20 ceiling" once the pool was divided —
    # whichever gateway happened to be last — which is exactly the kind of
    # confidently wrong summary this script exists to avoid.
    print(f"{len(gateways)} gateway(s) divide a ${pool:g} pool: "
          + ", ".join(f"{d} ${round(pool * s, 2):g}" for d, s in sorted(shares.items())))
    return 0


if __name__ == "__main__":
    sys.exit(main())
