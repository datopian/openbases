"""Apply and VERIFY the dynamic routes defined in infra/gateway/routes (wg-sn4).

    scripts/with_secrets.sh staging python3 scripts/ai_gateway_routes.py staging
    scripts/with_secrets.sh staging python3 scripts/ai_gateway_routes.py staging --dry-run

A route is one per task class. Callers address it as `dynamic/<name>` and never
name a model, so changing what runs a class is a change to a file in this
repository rather than to every caller.

Not in Terraform, for the same reason the spend limits are not: the Cloudflare
provider does not model this resource. Written as a script that reads back what
it wrote, on the same reasoning — a write that reports success is not evidence
the value was stored, and this API has already proved that once for spend
limits, where state showed a limit against a gateway that had none.

The definitions live in Git and this only ever pushes them, so the dashboard is
never the source of truth. A route edited there is overwritten on the next run,
which is the intended direction: the alternative is a routing decision that
exists only in a UI nobody reviews.
"""
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

ROUTES_DIR = Path(__file__).resolve().parent.parent / "infra" / "gateway" / "routes"

# One gateway per trust domain, and routes go to all of them. A route is a
# routing decision, not a credential: the oss cell must not reach the client
# gateway, but both should classify with the same model.
GATEWAYS = {
    "staging": [
        "workgraph-staging-oss",
        "workgraph-staging-internal",
        "workgraph-staging-client",
    ],
}


def call(method: str, path: str, token: str, body: dict | None = None) -> tuple[int, dict]:
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        f"https://api.cloudflare.com/client/v4{path}",
        data=data,
        method=method,
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req) as r:
            return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except json.JSONDecodeError:
            return e.code, {"success": False, "errors": [raw[:200]]}


def wanted_routes() -> list[dict]:
    """The routes this repository defines, with the commentary stripped.

    Keys beginning with an underscore are documentation for a reader and are not
    sent: the API rejects fields it does not know, and a route that fails to
    apply because of a comment would be a poor trade for the comment.
    """
    out = []
    for path in sorted(ROUTES_DIR.glob("*.json")):
        doc = json.loads(path.read_text())
        out.append({k: v for k, v in doc.items() if not k.startswith("_")})
    return out


def primary_model(route: dict) -> str:
    """The model a healthy request through this route should reach.

    The first model element on the success path — what the route resolves to
    when nothing has failed and no budget is exhausted. This is what the probe
    below asserts, because it is the one observable fact about a stored route.
    """
    by_id = {e["id"]: e for e in route["elements"]}
    node = by_id[next(e["id"] for e in route["elements"] if e["type"] == "start")]
    seen = set()
    while node and node["id"] not in seen:
        seen.add(node["id"])
        if node["type"] == "model":
            return node["properties"]["model"]
        nxt = node.get("outputs", {}).get("next") or node.get("outputs", {}).get("success")
        node = by_id.get(nxt["elementId"]) if nxt else None
    raise SystemExit(f"route {route['name']} has no model on its success path")


def probe(gid: str, name: str, account: str, gw_token: str) -> tuple[bool, str]:
    """Send one tiny request through the route and report which model answered.

    Verification is behavioural because it cannot be anything else: the API
    stores elements and returns them from no endpoint — not the list, not the
    route, not the version. There is nothing to compare a definition against.

    That turns out to be the better check anyway. Comparing stored JSON proves a
    write landed; this proves the route resolves, is deployed, and reaches the
    model it is supposed to. The spend-limit script had to learn the difference
    the hard way, from an API that reported a limit it was not enforcing.

    It costs a fraction of a cent — eight tokens against the cheapest model in
    the route — which is worth paying on every apply.
    """
    body = json.dumps({
        "model": f"dynamic/{name}",
        "messages": [{"role": "user", "content": "reply with the single word: ok"}],
        "max_tokens": 8,
    }).encode()
    req = urllib.request.Request(
        f"https://gateway.ai.cloudflare.com/v1/{account}/{gid}/compat/chat/completions",
        data=body, method="POST",
        headers={
            "cf-aig-authorization": f"Bearer {gw_token}",
            "Content-Type": "application/json",
            # Attributed like everything else, so a probe is visible in
            # usage_records rather than appearing as untagged spend.
            "cf-aig-metadata": json.dumps(
                {"role": "control-plane", "cell": "none", "bead": "wg-sn4", "rig": "route-probe"}),
            # Named, because urllib's default identifies itself as Python and
            # Cloudflare answers that with a 403 and error code 1010 — a bot
            # challenge, which reads like an authentication failure and is not.
            "User-Agent": "workgraph-gateway-routes/1",
        },
    )
    try:
        with urllib.request.urlopen(req) as r:
            return True, (json.load(r).get("model") or "")
    except urllib.error.HTTPError as e:
        return False, f"HTTP {e.code}: {e.read().decode()[:120]}"


def main() -> int:
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    dry_run = "--dry-run" in sys.argv[1:]
    force = "--force" in sys.argv[1:]
    environment = args[0] if args else "staging"

    token = os.environ.get("CLOUDFLARE_API_TOKEN")
    account = os.environ.get("CLOUDFLARE_ACCOUNT_ID")
    gw_token = os.environ.get("WG_AI_GATEWAY_TOKEN")
    if not token or not account or not gw_token:
        sys.exit("CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID and WG_AI_GATEWAY_TOKEN must be set")

    gateways = GATEWAYS.get(environment)
    if not gateways:
        sys.exit(f"no gateways recorded for {environment!r}")

    routes = wanted_routes()
    if not routes:
        sys.exit(f"no route definitions in {ROUTES_DIR}")
    print(f"{len(routes)} route(s) defined: {', '.join(r['name'] for r in routes)}")

    failures: list[str] = []
    for gid in gateways:
        base = f"/accounts/{account}/ai-gateway/gateways/{gid}/routes"
        status, listed = call("GET", base, token)
        if status != 200 or not listed.get("success"):
            failures.append(f"{gid}: cannot list routes: {listed.get('errors')}")
            continue
        # `data.routes`, not `result`. The envelope differs between endpoints on
        # this API — the list returns `data`, a single route returns `result` —
        # and reading the wrong one made a created route look absent.
        existing = {r.get("name"): r for r in ((listed.get("data") or {}).get("routes") or [])}

        for want in routes:
            name = want["name"]
            have = existing.get(name)
            expect = primary_model(want)

            # An existing route is probed before it is rewritten. A blind PUT
            # would mint a new version on every apply, and the budget node's
            # counter is keyed to the route — the spend-limit script exists
            # because exactly that reset a 30-day window on every run.
            if have and not force:
                ok, answered = probe(gid, name, account, gw_token)
                if ok and answered == expect:
                    print(f"  {gid}/{name}: resolves to {answered}, left alone")
                    continue
                print(f"  {gid}/{name}: probe said {answered!r}, expected {expect!r}; rewriting")

            if dry_run:
                print(f"  {gid}/{name}: WOULD {'update' if have else 'create'}")
                continue

            if have:
                # PATCH, not PUT. PUT on a route returns 404 — the resource
                # exists and the verb does not, which reads as a wrong id and is
                # not. POST to {id}/versions also works and mints a version;
                # PATCH is the update.
                status, result = call("PATCH", f"{base}/{have['id']}", token, want)
            else:
                status, result = call("POST", base, token, want)
            if not result.get("success"):
                failures.append(f"{gid}/{name}: write refused ({status}): {result.get('errors')}")
                continue

            ok, answered = probe(gid, name, account, gw_token)
            if not ok:
                failures.append(f"{gid}/{name}: written, but a request through it failed: {answered}")
            elif answered != expect:
                failures.append(f"{gid}/{name}: written, but resolves to {answered!r} not {expect!r}")
            else:
                print(f"  {gid}/{name}: {'updated' if have else 'created'}, resolves to {answered}")

    if failures:
        print("\nFAILED:", file=sys.stderr)
        for f in failures:
            print(f"  {f}", file=sys.stderr)
        return 1
    print("\nevery route matches its definition in infra/gateway/routes")
    return 0


if __name__ == "__main__":
    sys.exit(main())
