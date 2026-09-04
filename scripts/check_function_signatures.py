#!/usr/bin/env python3
"""A migration that REPLACES a function must drop the signature it creates.

The mistake this catches, twice made:

    DROP FUNCTION IF EXISTS f(text, text);          -- the old shape
    CREATE FUNCTION f(text, text, text[]) ...       -- the new one

Run once that is fine. Run twice -- which happens when a migration reaches a
database out of band, without its schema_migrations row -- the DROP removes
nothing and the CREATE collides:

    ERROR: function "f" already exists with same argument types (SQLSTATE 42723)

0078 did it, #199 fixed it and wrote down why, and 0083 then did it again. A rule
somebody knows is not a rule; this is the version that holds.

DELIBERATELY NARROW. A migration that creates a brand-new function name and
drops nothing is not flagged: it can only run once anyway, and twelve historical
migrations are in that shape. The signal is a file that drops SOME signatures of
a name and creates one it does not drop -- that file is replacing a function, so
somebody has already reasoned about re-running it and got it wrong.
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
MIGRATIONS = ROOT / "db" / "migrations"

problems = []

# Migrations that have this shape and cannot be corrected, with the reason.
#
# cmd/migrate refuses to run when an applied migration's checksum changes --
# "an applied migration must never be edited, write a new one" -- so a file
# already recorded in schema_migrations is frozen. Editing 0072 would break
# every deploy against every database that has it.
#
# The residual risk is narrow and worth stating: 0072 only fails if it is
# applied a SECOND time, which happens when a migration reaches a database out
# of band without its schema_migrations row. It is recorded everywhere it has
# run, so it will not re-run. A later migration may supersede it; this list is
# not permission to add more.
FROZEN = {
    "0072_plan_into_project.sql":
        "applied and recorded, so editing it would fail the checksum guard in "
        "cmd/migrate. It replaces system_enqueue_work without dropping the "
        "seven-argument form it creates, and would collide on a second pass.",
}


def args_of(text: str, open_paren: int) -> str:
    """Return the argument list starting at the given '(' , brace-matched."""
    depth = 0
    for i in range(open_paren, len(text)):
        if text[i] == "(":
            depth += 1
        elif text[i] == ")":
            depth -= 1
            if depth == 0:
                return text[open_paren + 1 : i]
    return ""


def split_args(args: str) -> list[str]:
    """Split on top-level commas, so `numeric(16,8)` stays one argument."""
    out, depth, current = [], 0, []
    for ch in args:
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
        if ch == "," and depth == 0:
            out.append("".join(current))
            current = []
            continue
        current.append(ch)
    if "".join(current).strip():
        out.append("".join(current))
    return [a.strip() for a in out if a.strip()]


def types_of(args: str, named: bool) -> str:
    """Normalise an argument list to a comparable type signature.

    `named` is True for CREATE, where each argument is `name type [DEFAULT x]`,
    and False for DROP, where it is just `type`.
    """
    types = []
    for arg in split_args(args):
        # DEFAULT and any mode keyword are not part of the identity.
        arg = re.split(r"(?i)\bDEFAULT\b", arg)[0].strip()
        arg = re.sub(r"(?i)^\s*(IN|OUT|INOUT|VARIADIC)\s+", "", arg).strip()
        if named:
            parts = arg.split(None, 1)
            arg = parts[1] if len(parts) == 2 else parts[0]
        types.append(re.sub(r"\s+", "", arg).lower())
    return ",".join(types)


for path in sorted(MIGRATIONS.glob("*.sql")):
    sql = path.read_text()

    dropped: dict[str, set[str]] = {}
    for m in re.finditer(r"(?is)DROP\s+FUNCTION\s+(?:IF\s+EXISTS\s+)?(\w+)\s*\(", sql):
        name = m.group(1).lower()
        sig = types_of(args_of(sql, m.end() - 1), named=False)
        dropped.setdefault(name, set()).add(sig)

    for m in re.finditer(r"(?is)CREATE\s+FUNCTION\s+(\w+)\s*\(", sql):
        name = m.group(1).lower()
        if name not in dropped:
            # A brand-new function name. It can only ever run once, and
            # flagging it would demand edits to migrations already applied.
            continue
        sig = types_of(args_of(sql, m.end() - 1), named=True)
        if sig not in dropped[name]:
            if path.name in FROZEN:
                continue
            problems.append(
                f"{path.name}: replaces {name} but does not drop the signature it creates.\n"
                f"      creates: {name}({sig})\n"
                f"      drops:   "
                + "\n               ".join(f"{name}({d})" for d in sorted(dropped[name]))
                + f"\n      Add: DROP FUNCTION IF EXISTS {name}({sig});\n"
                f"      Without it a second pass fails with SQLSTATE 42723 (0078, 0083)."
            )

if problems:
    print("function signature checks FAILED:")
    for p in problems:
        print(f"  - {p}")
    sys.exit(1)
for name, why in sorted(FROZEN.items()):
    print(f"  frozen: {name} — {why}")
print(f"function replacements drop the signature they create ({len(list(MIGRATIONS.glob('*.sql')))} file(s))")
