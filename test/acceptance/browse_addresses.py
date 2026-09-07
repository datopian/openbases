#!/usr/bin/env python3
"""wg-browse refuses the addresses an agent has no business fetching.

A browser hands an agent HTTP GET from inside the execution node, and that node
answers 200 on http://169.254.169.254/hetzner/v1/metadata. This is the check
that stands between an agent and cloud instance metadata, so it is tested
directly rather than trusted.

Run: python3 test/acceptance/browse_addresses.py
"""

import os
import sys
import types

TEMPLATE = os.path.join(
    os.path.dirname(os.path.abspath(__file__)),
    "..", "..", "infra", "ansible", "roles", "gastown", "templates",
    "wg-browse.py.j2")


def load():
    with open(TEMPLATE) as f:
        source = f.read().replace(
            "{{ chrome_headless_shell_binary }}",
            "/usr/local/bin/chrome-headless-shell")
    module = types.ModuleType("wg_browse")
    module.__dict__["__name__"] = "wg_browse_under_test"
    exec(compile(source, TEMPLATE, "exec"), module.__dict__)
    return module


def run(m):
    failures = []

    def refused(address, because):
        why = m.refuse(address)
        if why is None:
            failures.append(f"{address} was ALLOWED; it should be refused ({because})")
        elif because not in why:
            failures.append(f"{address} refused for {why!r}, expected mention of {because!r}")

    def allowed(address, because):
        why = m.refuse(address)
        if why is not None:
            failures.append(f"{address} was refused ({why}); it should be allowed ({because})")

    # The one that matters. Hetzner, AWS, GCP and Azure all serve instance
    # metadata from this address, and this node answers 200 on it.
    refused("169.254.169.254", "link-local")
    refused("169.254.1.1", "link-local")
    refused("fe80::1", "link-local")

    # The private ranges: the node's own network neighbours.
    for address in ("10.0.0.5", "172.16.4.1", "192.168.1.10", "fd00::1"):
        refused(address, "private network")

    refused("224.0.0.1", "not a routable public address")
    refused("0.0.0.0", "not a routable public address")
    refused("not-an-address", "not an address")

    # Loopback is ALLOWED, deliberately: looking at a dev server the run just
    # started is the reason this tool exists, and a browser that cannot reach
    # localhost cannot do the job. Asserted so the decision is recorded rather
    # than discovered by someone wondering why it works.
    allowed("127.0.0.1", "a dev server the run started")
    allowed("::1", "a dev server the run started")

    # Public addresses.
    allowed("93.184.216.34", "an ordinary public address")
    allowed("2606:2800:220:1:248:1893:25c8:1946", "an ordinary public address")

    # A scheme that is not http(s) is refused before anything resolves.
    for url in ("file:///etc/passwd", "ftp://example.com/x", "gopher://x"):
        try:
            m.check(url)
            failures.append(f"{url} was accepted; only http and https should be")
        except SystemExit as e:
            if "not a scheme" not in str(e):
                failures.append(f"{url} refused with {e!r}, expected a scheme complaint")

    # And a URL whose host resolves to a refused address is refused by check(),
    # not merely by refuse() — the two are wired together here.
    m.resolved = lambda host: ["169.254.169.254"] if host == "metadata.test" else []
    try:
        m.check("http://metadata.test/latest")
        failures.append("a host resolving to link-local was accepted")
    except SystemExit as e:
        if "link-local" not in str(e):
            failures.append(f"refused with {e!r}, expected a link-local complaint")

    # EVERY resolved address is checked, not just the first. A name that
    # resolves to both a public and a private address can be steered to the
    # private one, and checking only the first would let it through.
    m.resolved = lambda host: ["93.184.216.34", "10.1.2.3"]
    try:
        m.check("http://both.test/")
        failures.append("a host resolving to public AND private was accepted; "
                        "only the first address was checked")
    except SystemExit as e:
        if "private" not in str(e):
            failures.append(f"refused with {e!r}, expected a private-network complaint")

    # A host that does not resolve is refused rather than passed to the browser.
    m.resolved = lambda host: []
    try:
        m.check("http://nowhere.test/")
        failures.append("a host that does not resolve was accepted")
    except SystemExit as e:
        if "does not resolve" not in str(e):
            failures.append(f"refused with {e!r}, expected a resolution complaint")

    return failures


if __name__ == "__main__":
    problems = run(load())
    for p in problems:
        print(p, file=sys.stderr)
    if problems:
        print(f"\n{len(problems)} problem(s)", file=sys.stderr)
        sys.exit(1)
    print("wg-browse refuses metadata, private networks and non-http schemes; "
          "allows loopback and public")
