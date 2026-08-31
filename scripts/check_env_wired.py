#!/usr/bin/env python3
"""Every variable a deployed environment file sets must be read by the code.

WG_PUBSUB_PUSH_AUDIENCE was set in control-api.env for a week, consumed by
cmd/control-api to decide whether to register the Pub/Sub push endpoint, and
never read by config.LoadControlAPI. So the audience was always empty, the
endpoint was never registered, and every Google push fell through to the
authenticated mux and was refused.

Nothing looked wrong anywhere. The environment file set it, the struct declared
it, the consumer checked it, and the guard — "register only when an audience is
configured" — read as careful while behaving as "never register". The unit test
for the unregistered case asserted the same 401 as the registered case, so it
could not fail either.

This is the cheapest possible check for that whole class: a name written into a
deployed file that appears nowhere in the Go source is a setting that does
nothing.
"""
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent

# The environment files handed to a systemd unit. A variable here is a promise
# that the process reads it.
TEMPLATES = [
    "infra/ansible/roles/control_api/templates/control-api.env.j2",
]

# Names read by something other than Go: a shell script, a systemd directive, or
# the template's own logic. Each needs a reason, because "it is read elsewhere"
# is exactly what was believed about the audience.
ELSEWHERE = {
    # Read by systemd itself rather than by our code.
    "WG_DATABASE_URL_TEMPLATE": "assembled by config.DatabaseURL from the credential",
}


def main() -> int:
    problems = []
    checked = 0

    for rel in TEMPLATES:
        path = ROOT / rel
        if not path.exists():
            problems.append(f"{rel} does not exist; update TEMPLATES in this script")
            continue

        for line in path.read_text().splitlines():
            m = re.match(r"^(WG_[A-Z0-9_]+)=", line.strip())
            if not m:
                continue
            name = m.group(1)
            checked += 1
            if name in ELSEWHERE:
                continue
            # Searched across the whole Go tree rather than one package: a
            # variable may legitimately be read by any binary.
            found = subprocess.run(
                ["git", "grep", "-l", "--", name, "--", "*.go"],
                cwd=ROOT, capture_output=True, text=True,
            )
            if found.returncode != 0 or not found.stdout.strip():
                problems.append(
                    f"{rel} sets {name}, but no Go file mentions it — so the setting "
                    f"does nothing. Either read it, or remove it from the template, or "
                    f"record it in ELSEWHERE with the reason."
                )

    print(f"deployed environment variables ({checked} checked)")
    if problems:
        print("FAILED:")
        for p in problems:
            print(f"  - {p}")
        return 1
    print("  every deployed variable is read by the code")
    return 0


if __name__ == "__main__":
    sys.exit(main())
