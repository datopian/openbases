"""Compare the recorded model limits against Cloudflare's own catalogue.

    scripts/with_secrets.sh staging python3 scripts/check_model_limits.py

Context windows are copied into two places — GatewayModels in
internal/runner/plan.go and wg_model_limits in the execution_cell role — because
a planner that makes a network call has a new failure mode. Copies rot, and
these copies have been wrong twice.

The first time, three of seven figures were written from assumption and all were
too small; gemma-4-26b was recorded at 16,384 against a real 256,000 and was
dropped from a model bake-off because of it. The second time, reading the
documentation by hand still put glm-5.3-flash at 1,048,576 against a real
1,310,720.

A wrong-low context does not fail loudly. It caps what an agent can see, so the
run simply does less well, and nothing in the output says so. Hence a check.

Not in `make check`: it needs Cloudflare credentials, and CI has none. Run it
when adding a model, and when a run seems to have less room than it should.
"""
import json
import os
import re
import sys
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
ANSIBLE = ROOT / "infra" / "ansible" / "roles" / "execution_cell" / "defaults" / "main.yml"
GO = ROOT / "internal" / "runner" / "plan.go"

# Only Workers AI models are in Cloudflare's catalogue. Anthropic models reach
# the gateway through a provider path and their limits come from Anthropic, so
# there is nothing here to compare them against.
WORKERS_AI = "workers-ai/@cf/"


def account_catalogue(account: str, token: str) -> dict[str, int]:
    url = f"https://api.cloudflare.com/client/v4/accounts/{account}/ai/models/search?per_page=200"
    req = urllib.request.Request(url, headers={
        "Authorization": f"Bearer {token}",
        # Named: Cloudflare answers an unnamed client with a bot challenge.
        "User-Agent": "workgraph-model-limits/1",
    })
    with urllib.request.urlopen(req) as r:
        doc = json.load(r)
    out = {}
    for model in doc.get("result") or []:
        props = {p.get("property_id"): p.get("value") for p in (model.get("properties") or [])}
        if "context_window" in props:
            out[model["name"]] = int(props["context_window"])
    return out


def recorded_ansible() -> dict[str, int]:
    """Parse wg_model_limits out of the role defaults.

    A regex rather than a YAML parser, so this script has no dependencies — the
    block is a flat map of one-line entries and has to stay that way for this to
    work, which is a fair trade for it running anywhere.
    """
    text = ANSIBLE.read_text()
    block = text.split("wg_model_limits:", 1)
    if len(block) < 2:
        return {}
    out = {}
    for line in block[1].splitlines():
        m = re.match(r"\s+(\S+):\s*\{context:\s*(\d+)", line)
        if m:
            out[m.group(1)] = int(m.group(2))
        elif line.strip() and not line.startswith((" ", "\t")):
            break
    return out


def recorded_go() -> dict[str, int]:
    out = {}
    for m in re.finditer(r'"([^"]+)":\s*\{Context:\s*(\d+)', GO.read_text()):
        out[m.group(1)] = int(m.group(2))
    return out


def main() -> int:
    token = os.environ.get("CLOUDFLARE_API_TOKEN")
    account = os.environ.get("CLOUDFLARE_ACCOUNT_ID")
    if not token or not account:
        sys.exit("CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID must be set")

    catalogue = account_catalogue(account, token)
    print(f"{len(catalogue)} model(s) in the account catalogue")

    problems = 0
    for label, recorded in (("Ansible", recorded_ansible()), ("Go", recorded_go())):
        for name, ctx in sorted(recorded.items()):
            if not name.startswith(WORKERS_AI):
                continue
            cf_name = name[len("workers-ai/"):]
            real = catalogue.get(cf_name)
            if real is None:
                # Not an error on its own: a model can be recorded before the
                # account is granted it, which is exactly kimi-k3's position.
                print(f"  note     {label:8s} {cf_name:44s} recorded {ctx:>9,}, not in the account catalogue")
                continue
            if real != ctx:
                print(f"  MISMATCH {label:8s} {cf_name:44s} recorded {ctx:>9,}, account says {real:>9,}")
                problems += 1
            else:
                print(f"  ok       {label:8s} {cf_name:44s} {ctx:>9,}")

    if problems:
        print(f"\n{problems} limit(s) disagree with Cloudflare. A wrong-low context does not fail "
              f"loudly — it caps what the agent can see.", file=sys.stderr)
        return 1
    print("\nevery recorded Workers AI limit matches the account catalogue")
    return 0


if __name__ == "__main__":
    sys.exit(main())
