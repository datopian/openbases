#!/usr/bin/env python3
"""Client and operational data belongs in the deployment, not in this repository.

The repository is public. Two kinds of thing must not enter it, and neither is a
credential, so neither is caught by gitleaks or by GitHub push protection:

  1. Live external identifiers -- Google Meet space codes and ids, Drive ids,
     and the addresses of real staff. None grants access on its own; all of them
     name real meetings, real drives and real people, and a public repository is
     an index that never forgets.

  2. Business records seeded from a migration. A migration is schema and
     backfill (db/migrations/README.md rule 4). The moment one INSERTs a client
     project, that client's name is in the source tree forever -- the migration
     is applied, cmd/migrate refuses to let it change, and the only remedy is a
     history rewrite. This is exactly how `nged` and `cdt` got here.

Where the data goes instead: the deployment's own database. Seed it with an
idempotent bootstrap against the live schema, or through the API, so the record
lives in persistent storage and the repository carries only the shape.

## The baseline

Everything above already happened, in migrations that cannot now be edited. So
this check compares against scripts/disclosure_baseline.txt and fails only on
what is NEW. A baseline entry is a SHA-256 prefix of the offending token, never
the token, because a file that lists what must not be published would be the
same mistake one level up.

Add to the baseline only what is already applied and immutable, or what you have
read and know to be a fixture. A fingerprint covers path AND token, so a NEW
identifier in an already-baselined file still fails -- which is the point, and is
what makes it safe to baseline a synthetic test fixture.

The Meet-code rule collides with ordinary hyphenated English: a space code is
three-four-three lowercase letters, and so is "two-hour-old". Read the finding
before baselining it rather than reaching for --update.

    python3 scripts/check_disclosure.py            # check
    python3 scripts/check_disclosure.py --update    # re-record the baseline
"""
import hashlib
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
BASELINE = ROOT / "scripts" / "disclosure_baseline.txt"
MANIFEST = ROOT / "db" / "tenant_seeds.txt"

# A Meet space code is three-four-three lowercase letters. The lookarounds are
# load-bearing: without them "remote-code-execution" contains "ote-code-exe" and
# every ADR mentioning it fails the check.
MEET_CODE = re.compile(r"(?<![A-Za-z0-9-])[a-z]{3}-[a-z]{4}-[a-z]{3}(?![A-Za-z0-9-])")

RULES = [
    ("meet-code", MEET_CODE),
    # Shared drive ids are stable and begin 0A.
    ("drive-id", re.compile(r"(?<![A-Za-z0-9_-])0A[A-Za-z0-9_-]{15,}(?![A-Za-z0-9_-])")),
    # A Meet space id, in the form the API uses.
    ("meet-space", re.compile(r"spaces/[A-Za-z0-9_-]{8,}")),
    # first.last@datopian.com is a real person. Test fixtures use example.com,
    # .invalid, or a single word, and are left alone.
    ("staff-email", re.compile(r"(?<![A-Za-z0-9._%+-])[a-z]+\.[a-z]+@datopian\.com")),
]

# Tables holding records about the real world. Reference and lookup tables are
# not here: `roles` defines the permission model and belongs in the schema.
BUSINESS_TABLES = {
    "projects", "project_memberships", "project_repositories", "portfolios",
    "users", "identities", "role_grants", "event_sources", "source_acl_entries",
    "beads_databases", "work_refs", "work_queue", "execution_nodes",
    "execution_cells", "execution_rigs", "agent_profiles", "usage_records",
    "attention_items", "credential_registry", "knowledge_records",
    "knowledge_reviews", "knowledge_record_sources", "audit_log",
    "event_receipts", "usage_import_runs",
}
INSERT = re.compile(r"INSERT\s+INTO\s+([a-z_]+)", re.IGNORECASE)

# A dollar-quoting tag: $$, $function$, $_$.
DOLLAR = re.compile(r"\$[a-zA-Z_][a-zA-Z_0-9]*\$|\$\$")
ENDS_WITH_DO = re.compile(r"(?i)\bDO\s*$")


def statements(sql):
    """The SQL that runs when the migration is APPLIED.

    A CREATE FUNCTION body is removed: it runs when the function is called, so
    it is not a seed. Without this the check refused migration 0091, whose only
    purpose is the function that lets a NEW deployment seed its own first
    administrator -- precisely backwards.

    A `DO $$ ... $$` block is KEPT, because it executes immediately and an
    insert inside one is a seed like any other. The first version of this
    stripped both, and that made the rule blind to exactly what it is for:
    0080_restructure_projects.sql seeds through a DO block. cmd/migrate's test
    draws the same distinction, and the two must agree -- when they disagreed,
    45 entries had already been baselined by mistake.
    """
    out = []
    pos = 0
    while True:
        opening = DOLLAR.search(sql, pos)
        if not opening:
            out.append(sql[pos:])
            return "\n".join(out)
        closing = DOLLAR.search(sql, opening.end())
        if not closing:  # unterminated; keep the rest rather than lose it
            out.append(sql[pos:])
            return "\n".join(out)
        out.append(sql[pos:opening.start()])
        if ENDS_WITH_DO.search(sql[:opening.start()]):
            out.append(sql[opening.end():closing.start()])
        pos = closing.end()


# These three necessarily contain the patterns: this file describes them, the
# baseline records them by hash, and the test has to carry a realistic
# identifier to prove the check refuses one.
EXEMPT = {
    "scripts/check_disclosure.py",
    "scripts/disclosure_baseline.txt",
    "test/acceptance/disclosure_check.py",
}

SKIP_SUFFIX = (".lock", ".png", ".jpg", ".gif", ".pdf", ".ico", ".woff", ".woff2")

# SOPS ciphertext is base64 and collides with the drive-id shape by chance.
# check_secrets_encrypted.py owns these files and checks the thing that matters,
# which is that they are encrypted at all.
SKIP_PREFIX = ("infra/secrets/",)


def tracked():
    out = subprocess.run(["git", "ls-files", "-z"], cwd=ROOT,
                         capture_output=True, text=True, check=True).stdout
    return [p for p in out.split("\0") if p]


def fingerprint(path, rule, token):
    h = hashlib.sha256(token.encode()).hexdigest()[:12]
    return f"{path}:{rule}:{h}"


def findings():
    found = {}
    for rel in tracked():
        if rel in EXEMPT or rel.endswith(SKIP_SUFFIX) or rel.startswith(SKIP_PREFIX):
            continue
        p = ROOT / rel
        try:
            text = p.read_text(encoding="utf-8")
        except (UnicodeDecodeError, FileNotFoundError, IsADirectoryError):
            continue

        for name, pattern in RULES:
            for m in set(pattern.findall(text)):
                found[fingerprint(rel, name, m)] = (rel, name, m)

        # A migration seeds business data. Checked on the directory rather than
        # the filename so a seed cannot hide under a different suffix.
        if rel.startswith("db/migrations/") and rel.endswith(".sql"):
            for table in {t.lower() for t in INSERT.findall(statements(text))}:
                if table in BUSINESS_TABLES:
                    found[fingerprint(rel, "seeded-record", table)] = (
                        rel, "seeded-record", table)
    return found


def load_baseline():
    if not BASELINE.exists():
        return set()
    return {ln.strip() for ln in BASELINE.read_text().splitlines()
            if ln.strip() and not ln.startswith("#")}


EXPLAIN = {
    "meet-code": "a Google Meet space code",
    "drive-id": "a Google shared-drive id",
    "meet-space": "a Google Meet space id",
    "staff-email": "the address of a real person",
    "seeded-record": "a business record seeded from a migration",
}

WHERE_INSTEAD = {
    "seeded-record": (
        "A migration carries schema and backfill only. Seed the record into the\n"
        "  deployment's database -- an idempotent bootstrap, or the API -- so the\n"
        "  record lives in persistent storage and the repository carries the shape.\n"
        "  An applied migration cannot be edited afterwards (cmd/migrate/main.go),\n"
        "  so this is not fixable later."),
}
DEFAULT_WHERE = (
    "Keep the identifier in the database (event_sources) or in deployment\n"
    "  configuration, and refer to it here by name or by role instead.")


def seed_manifest():
    """The migrations `migrate -fresh` skips, from db/tenant_seeds.txt."""
    if not MANIFEST.exists():
        return None
    return {ln.strip() for ln in MANIFEST.read_text().splitlines()
            if ln.strip() and not ln.startswith("#")}


def manifest_drift(found):
    """Where this check and db/tenant_seeds.txt disagree about what seeds.

    They answer the same question for different purposes -- one refuses new
    seeding, the other decides what a fresh install skips -- so they must name
    the same files. When they disagreed, both were wrong and neither said so:
    0027 and 0052 were in the manifest and seed nothing, which stripped four
    registration functions out of every fresh install, while 0018 and 0028
    seed credential_registry and were missing from it, so ten Datopian
    addresses shipped in an install that reported itself clean.

    That was found by comparing the two lists by hand. This is that comparison.
    """
    manifest = seed_manifest()
    if manifest is None:
        return ["db/tenant_seeds.txt is missing; `migrate -fresh` would skip nothing"]

    seeding = {rel.split("/")[-1] for rel, rule, _ in found.values()
               if rule == "seeded-record"}

    problems = []
    for name in sorted(seeding - manifest):
        problems.append(
            f"{name} seeds a record but is NOT in db/tenant_seeds.txt, so a fresh "
            f"install would contain it")
    for name in sorted(manifest - seeding):
        problems.append(
            f"{name} is in db/tenant_seeds.txt but seeds nothing. If it carries "
            f"schema, `migrate -fresh` is removing that schema from every fresh "
            f"install (cmd/migrate's tests check for DDL); if it is simply "
            f"obsolete, take it out")
    return problems


def main():
    found = findings()
    if "--update" in sys.argv:
        lines = ["# Accepted disclosures, recorded as SHA-256 prefixes so this file",
                 "# discloses nothing itself. See scripts/check_disclosure.py.",
                 "#", "# Add only what is already applied and immutable.", ""]
        lines += sorted(found)
        BASELINE.write_text("\n".join(lines) + "\n")
        print(f"recorded {len(found)} accepted disclosures in {BASELINE.name}")
        return 0

    baseline = load_baseline()
    # Checked before the baseline: this is a disagreement between two guards,
    # not a disclosure, and no baseline entry should ever silence it.
    drift = manifest_drift(found)
    if drift:
        print("db/tenant_seeds.txt and this check disagree about what seeds:\n",
              file=sys.stderr)
        for p in drift:
            print(f"  {p}\n", file=sys.stderr)
        return 1

    new = sorted(k for k in found if k not in baseline)

    if not new:
        # A baseline that only ever grows stops describing anything. Entries
        # for findings that no longer occur are how it rots: the file keeps
        # excusing things nobody has checked in months, and the next real
        # finding hides among them. Reported once the tree is otherwise clean,
        # so the fix cannot absorb a finding by accident.
        stale = sorted(baseline - set(found))
        if stale:
            print(f"{len(stale)} baseline entr{'y' if len(stale) == 1 else 'ies'} "
                  f"no longer match anything:", file=sys.stderr)
            for k in stale[:10]:
                print(f"  {k}", file=sys.stderr)
            if len(stale) > 10:
                print(f"  ... and {len(stale) - 10} more", file=sys.stderr)
            print("\nThe tree is otherwise clean, so re-recording is safe:",
                  file=sys.stderr)
            print("  python3 scripts/check_disclosure.py --update", file=sys.stderr)
            return 1
        print(f"no new disclosures ({len(found)} accepted, baselined)")
        return 0

    print("NEW disclosure in tracked files. The repository is public.\n",
          file=sys.stderr)
    for key in new:
        rel, rule, token = found[key]
        shown = token if rule == "seeded-record" else token[:4] + "..."
        print(f"  {rel}\n    {EXPLAIN[rule]}: {shown}", file=sys.stderr)
        print(f"  {WHERE_INSTEAD.get(rule, DEFAULT_WHERE)}\n", file=sys.stderr)
    print("If this is already applied and cannot be changed, record it:",
          file=sys.stderr)
    print("  python3 scripts/check_disclosure.py --update", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
