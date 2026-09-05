#!/usr/bin/env python3
"""The rig provisioner reports the useful part of a failure.

This function exists because I got it wrong twice on the same deploy, in
opposite directions, and each time the deploy log said something true and
useless. Both real outputs are the fixtures below.

Run: python3 test/acceptance/provisioner_reason.py
"""

import os
import subprocess
import sys
import types

TEMPLATE = os.path.join(
    os.path.dirname(os.path.abspath(__file__)),
    "..", "..", "infra", "ansible", "roles", "execution_cell",
    "templates", "wg-provision-rigs.py.j2")


def load():
    """Load the template as a module, with its one substitution resolved.

    Rendering by hand rather than with jinja2: the template has a single
    variable and requiring a python package to test a deploy script would put
    the test behind an install that CI would then have to carry.
    """
    with open(TEMPLATE) as f:
        source = f.read().replace("{{ cells_root }}", "/srv/cells")
    module = types.ModuleType("provisioner")
    module.__dict__["__name__"] = "provisioner_under_test"
    exec(compile(source, TEMPLATE, "exec"), module.__dict__)
    return module


def run(module):
    failures = []

    def check(name, stderr, must_say, must_not_say):
        got = module.reason(subprocess.CompletedProcess(
            args=["gt"], returncode=1, stdout="", stderr=stderr))
        for want in must_say:
            if want not in got:
                failures.append(f"{name}: does not say {want!r}\n  got: {got}")
        for unwanted in must_not_say:
            if unwanted in got:
                failures.append(f"{name}: still says {unwanted!r}\n  got: {got}")

    # gt's own error comes FIRST, followed by its entire usage text. Printing
    # everything buried the one useful line in forty lines of flags.
    check(
        "gt refuses outside a town",
        "Error: not in a Gas Town workspace: not in a Gas Town workspace\n"
        "Usage:\n"
        "  gt rig add <name> <git-url> [flags]\n"
        "\n"
        "Flags:\n"
        "      --adopt                     Adopt an existing directory\n"
        "      --sparse-checkout strings   Sparse checkout paths\n",
        must_say=["not in a Gas Town workspace"],
        must_not_say=["--sparse-checkout", "Flags:", "Usage:"])

    # A git failure underneath gt comes LAST, after progress output. Printing
    # the first line reported "Cloning into bare repository...", which is not
    # an error at all, and hid the reason entirely.
    check(
        "a clone that cannot authenticate",
        "Error: adding rig: creating bare repo: git clone: "
        "Cloning into bare repository '/tmp/gt-clone-1397497549/.repo.git'...\n"
        "git-credential-workgraph: no token issued for data-portal-examples\n"
        "fatal: could not read Username for "
        "'https://github.com/datopian/data-portal-examples.git': "
        "No such device or address\n",
        must_say=["no token issued for data-portal-examples", "could not read Username"],
        must_not_say=[])

    # A long clone must not paste its whole progress into an ansible message,
    # and what survives has to be the end, where the failure is.
    noisy = "\n".join([f"Receiving objects: {i}%" for i in range(60)] +
                      ["fatal: the remote end hung up unexpectedly"])
    check("a noisy clone is capped",
          noisy,
          must_say=["fatal: the remote end hung up unexpectedly", "..."],
          must_not_say=["Receiving objects: 3%"])

    # Nothing on either stream is still an answer, not an empty string.
    silent = module.reason(subprocess.CompletedProcess(
        args=["gt"], returncode=128, stdout="", stderr=""))
    if "128" not in silent:
        failures.append(f"a silent failure does not report its exit status: {silent!r}")

    return failures


if __name__ == "__main__":
    problems = run(load())
    for p in problems:
        print(p, file=sys.stderr)
    if problems:
        print(f"\n{len(problems)} problem(s)", file=sys.stderr)
        sys.exit(1)
    print("the provisioner reports the useful part of a failure")
