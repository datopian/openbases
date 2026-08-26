#!/usr/bin/env python3
"""An integration test that reads an RLS-protected table must drop privileges.

CI runs psql as `postgres`, a superuser. A superuser BYPASSES row-level security
entirely — not "is allowed by the policies", but never evaluates them. So a test
that queries a protected table as postgres sees every row whatever the policies
say, and an assertion like "a non-member cannot read this project" passes
identically whether the policy is correct, wrong, or absent.

That is the worst kind of test: it reports success about the property the whole
permission model rests on, and it cannot fail. wg-8yv.50 exists because the
suite was in that state.

The fix in each test is `SET ROLE workgraph_app`, which drops to the role the
application actually uses. It is NOLOGIN and cannot connect directly, but SET
ROLE from a superuser session applies its attributes — and it has no BYPASSRLS,
so the policies are evaluated.

This checker flags a test that reads a protected table without ever doing that.
It is deliberately conservative: it looks for the statement anywhere in the file
rather than trying to prove the queries come after it, because a false alarm
here costs a reader thirty seconds and a missed one costs the guarantee.
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
MIGRATIONS = ROOT / "db" / "migrations"
TESTS = ROOT / "test" / "integration"

# Tests that legitimately run as the owner, with the reason. An entry here is a
# claim that the file asserts nothing about who may READ a protected table.
OWNER_ONLY = {
    "invariants.sql":
        "asserts CHECK constraints and triggers, which apply to every role "
        "including the owner. RLS is not the property under test.",
    "knowledge_invariants.sql":
        "same: constraint and trigger behaviour, not visibility.",
    "identity_linking.sql":
        "asserts first-login identity linking, which happens before any "
        "project scoping exists to test.",
    "credential_registry.sql":
        "asserts the value-rejection trigger and the overdue view's arithmetic. "
        "The admin-only read policy is NOT covered here and is worth its own "
        "test; recorded rather than hidden.",
    "approvals.sql":
        "asserts the approval triggers — self-approval, digest binding, "
        "one-vote-per-approver, append-only decisions. It reads `projects` "
        "only to resolve a fixture id; no assertion concerns visibility.",
    "budgets.sql":
        "asserts budget resolution and summation through SECURITY DEFINER "
        "functions that run WITHOUT a user by design, and reads back what they "
        "wrote. usage_records visibility IS covered, under a real non-superuser "
        "role, by test/acceptance/cost_import.sh.",
    "execution_registry.sql":
        "asserts the deploy-time registration functions and that attribution "
        "refuses to guess when a cell hosts two projects. Every write goes "
        "through a SECURITY DEFINER function that runs WITHOUT a user by "
        "design, and the reads verify what those functions wrote, not what a "
        "user may see. usage_records visibility IS covered, under a real "
        "non-superuser role, by test/acceptance/cost_import.sh.",
    "hq_graph.sql":
        "asserts work_refs identity when execution_cell_id IS NULL and the "
        "link-layer constraints. Uniqueness and triggers, not visibility.",
    "agent_health.sql":
        "asserts deduplication counts and that recipients are resolved, which "
        "requires seeing rows across users on purpose. The inbox's own read "
        "policy is covered by rls_isolation.sql.",
}


def rls_tables() -> set[str]:
    tables = set()
    for path in sorted(MIGRATIONS.glob("*.sql")):
        for m in re.finditer(
            r"ALTER\s+TABLE\s+(\w+)\s+ENABLE\s+ROW\s+LEVEL\s+SECURITY", path.read_text(), re.I
        ):
            tables.add(m.group(1))
    return tables


def reads(sql: str, table: str) -> bool:
    """Does this file read the table in a query, rather than only alter it?"""
    return bool(re.search(rf"\b(?:FROM|JOIN)\s+{table}\b", sql, re.I))


def main() -> int:
    protected = rls_tables()
    if not protected:
        print("no RLS-enabled tables found; is this the right repository?", file=sys.stderr)
        return 1

    problems, checked = [], 0
    print(f"RLS test privilege check ({len(protected)} protected table(s))")

    for path in sorted(TESTS.glob("*.sql")):
        if path.name == "assert_app_role.sql":
            continue  # the guard itself, included by the tests rather than run
        sql = path.read_text()
        touched = sorted(t for t in protected if reads(sql, t))
        if not touched:
            continue
        checked += 1

        drops = re.search(r"SET\s+(?:LOCAL\s+)?ROLE\s+workgraph_app", sql, re.I)
        if drops:
            # Dropping is not enough on its own. `SET LOCAL` outside a
            # transaction block is a WARNING that applies nothing, and a SET
            # removed in a refactor leaves the suite green while every
            # visibility assertion below it becomes unfailable. So a test that
            # drops must also PROVE it dropped.
            if not re.search(r"\\ir\s+assert_app_role\.sql|<>\s*'workgraph_app'", sql):
                problems.append(
                    f"{path.relative_to(ROOT)} drops to workgraph_app but never asserts it "
                    f"took effect. Add `\\ir assert_app_role.sql` after the SET, or the "
                    f"inline current_user check if the SET is inside a PL/pgSQL block. "
                    f"Without it, deleting one line silently turns every visibility "
                    f"assertion below into a test that cannot fail."
                )
            continue
        if path.name in OWNER_ONLY:
            print(f"  {path.name}: owner by design — {OWNER_ONLY[path.name]}")
            continue

        problems.append(
            f"{path.relative_to(ROOT)} reads {', '.join(touched)} as a superuser, so "
            f"row-level security is bypassed and any visibility assertion in it cannot "
            f"fail. Add `SET ROLE workgraph_app` before the assertions, or record it in "
            f"OWNER_ONLY with the reason it does not test visibility."
        )

    print(f"  {checked} test(s) read a protected table")
    if problems:
        print("FAILED:")
        for p in problems:
            print(f"  - {p}")
        return 1
    print("every test that reads a protected table drops privileges or says why not")
    return 0


if __name__ == "__main__":
    sys.exit(main())
