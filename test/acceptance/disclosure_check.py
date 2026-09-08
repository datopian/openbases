#!/usr/bin/env python3
"""The disclosure check actually refuses new client and operational data.

A guardrail nobody tested is a guardrail nobody knows works, and this one has a
specific way of failing quietly: the baseline. Record too much and every future
occurrence is accepted; key the baseline on the path alone and a real identifier
dropped into an already-baselined test file sails through. That second case is
the one worth a test, because it is the shape of the mistake that put live
identifiers in this repository in the first place.

Runs against a temporary clone of the working tree, so it never leaves a
scratch file behind in a real one.

Run: python3 test/acceptance/disclosure_check.py
"""

import os
import pathlib
import shutil
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent.parent.parent
CHECK = "scripts/check_disclosure.py"


def run(tree):
    """Exit code of the check inside `tree`."""
    return subprocess.run([sys.executable, CHECK], cwd=tree,
                          capture_output=True, text=True)


def clone():
    """A throwaway copy of the tree as it stands, with git so ls-files works.

    A copy of the WORKING tree, not a clone of HEAD. The check is run by a
    pre-push hook and by CI against files that may not be committed yet, so
    testing HEAD would test the wrong thing -- and on the commit that
    introduces the check, HEAD does not contain it at all.
    """
    tmp = tempfile.mkdtemp(prefix="disclosure-")
    listed = subprocess.run(["git", "ls-files", "-z"], cwd=ROOT,
                            capture_output=True, text=True, check=True).stdout
    for rel in (x for x in listed.split("\0") if x):
        src, dst = ROOT / rel, pathlib.Path(tmp) / rel
        if not src.exists():          # deleted but still indexed
            continue
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(src, dst)
    # The check and its baseline are what is under test, and on the commit that
    # introduces them they are not tracked yet -- which made this test fail with
    # "can't open file", a message about the test rather than about the check.
    for rel in (CHECK, "scripts/disclosure_baseline.txt"):
        src, dst = ROOT / rel, pathlib.Path(tmp) / rel
        if src.exists():
            dst.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(src, dst)

    subprocess.run(["git", "init", "--quiet"], cwd=tmp, check=True)
    subprocess.run(["git", "add", "-A"], cwd=tmp, check=True,
                   capture_output=True)
    # Commit, or `git reset` between cases has no HEAD to reset TO and clears
    # the whole index instead. Every case after the first then ran against a
    # tree where almost nothing was tracked, and still passed, because the
    # exit code it expected happened to be produced by a different failure.
    # A fixture that quietly stops being a fixture is worse than no fixture.
    subprocess.run(["git", "-c", "user.email=t@example.invalid",
                    "-c", "user.name=t", "commit", "--quiet", "-m", "fixture"],
                   cwd=tmp, check=True, capture_output=True)
    return tmp


def case(tree, name, write, want, because=None):
    """Apply `write`, assert the exit code and the REASON, undo it.

    `because` is a string the failure must mention. Checking only the exit code
    is how this test spent its first version green while measuring nothing: a
    broken fixture cleared the index between cases, every later case ran against
    an almost-empty tree, and the 1 they expected was produced by a different
    complaint entirely. An exit code is not a reason.
    """
    path, body, mode = write
    p = pathlib.Path(tree) / path
    before = p.read_text() if p.exists() and mode == "a" else None
    with open(p, mode) as fh:
        fh.write(body)
    # Untracked files are invisible to git ls-files until intent-to-add.
    subprocess.run(["git", "add", "-N", path], cwd=tree, capture_output=True)

    # The fixture has to still BE the fixture. If the tree stops being tracked
    # the check sees nothing and its silence looks like agreement.
    listed = subprocess.run(["git", "ls-files"], cwd=tree,
                            capture_output=True, text=True).stdout.split()
    intact = len(listed) > 100

    got = run(tree)
    ok = got.returncode == want and intact
    if ok and because is not None:
        ok = because in (got.stdout + got.stderr)
    if before is None:
        p.unlink()
    else:
        p.write_text(before)
    subprocess.run(["git", "reset", "--quiet"], cwd=tree, capture_output=True)

    if not ok:
        detail = ""
        if not intact:
            detail = (f"\n    the fixture tree has only {len(listed)} tracked "
                      f"file(s); the check was measuring nothing")
        elif because is not None and because not in (got.stdout + got.stderr):
            detail = f"\n    the failure never mentions {because!r}"
        return (f"{name}: exit {got.returncode}, wanted {want}{detail}\n"
                f"    stdout: {got.stdout.strip()[:200]}\n"
                f"    stderr: {got.stderr.strip()[:200]}")
    return None


def main():
    tree = clone()
    problems = []
    try:
        # The tree as committed must pass, or every other case is meaningless.
        baseline = run(tree)
        if baseline.returncode != 0:
            problems.append(
                "the committed tree does not pass its own check: "
                f"exit {baseline.returncode}\n    {baseline.stderr.strip()[:300]}")

        problems.append(case(
            tree, "a drive id in a new file is refused",
            ("internal/workspace/probe_scratch.go",
             'package workspace\n\nvar probe = "0AZzYyXxWwVvUuTtSs"\n', "w"), 1,
            because="shared-drive id"))

        # The case the baseline could hide: the file is already accepted, the
        # identifier is not.
        problems.append(case(
            tree, "a NEW drive id in an already-baselined file is refused",
            ("internal/workspace/lifecycle_test.go",
             '\n// var probe = "0AQqWwEeRrTtYyUuIi"\n', "a"), 1,
            because="shared-drive id"))

        problems.append(case(
            tree, "a Meet space code in a new file is refused",
            ("docs/scratch_probe.md", "code zzz-yyyy-xxx\n", "w"), 1,
            because="Meet space code"))

        # A new seeding migration trips the manifest cross-check first, and
        # that is the better message: it names the file and says a fresh
        # install would contain it. Asserted as the reason rather than the
        # disclosure wording, because that is what actually fires -- guessing
        # produced a case that passed on the wrong complaint.
        problems.append(case(
            tree, "a migration seeding a business record is refused",
            ("db/migrations/0099_probe_scratch.sql",
             "INSERT INTO projects (slug) VALUES ('acme');\n", "w"), 1,
            because="NOT in db/tenant_seeds.txt"))

        # Schema is the whole point of a migration and must stay allowed, or the
        # check is one people route around.
        problems.append(case(
            tree, "a migration that only changes schema is allowed",
            ("db/migrations/0099_probe_scratch.sql",
             "ALTER TABLE projects ADD COLUMN probe text;\n", "w"), 0))
    finally:
        shutil.rmtree(tree, ignore_errors=True)

    return [p for p in problems if p]


if __name__ == "__main__":
    found = main()
    for p in found:
        print(p, file=sys.stderr)
    if found:
        print(f"\n{len(found)} problem(s)", file=sys.stderr)
        sys.exit(1)
    print("the disclosure check refuses new identifiers and seeded records")
