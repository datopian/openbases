#!/usr/bin/env python3
"""Reject SQL reserved words used as bare identifiers in migrations.

PostgreSQL accepts `system_user` in a CREATE TABLE only when quoted, and the
failure surfaces at apply time rather than at review time. This check moves it
to review time.
"""
import pathlib
import re
import sys

RESERVED = {
    "all", "analyse", "analyze", "and", "any", "array", "as", "asc", "asymmetric",
    "authorization", "binary", "both", "case", "cast", "check", "collate",
    "collation", "column", "concurrently", "constraint", "create", "cross",
    "current_catalog", "current_date", "current_role", "current_schema",
    "current_time", "current_timestamp", "current_user", "default", "deferrable",
    "desc", "distinct", "do", "else", "end", "except", "false", "fetch", "for",
    "foreign", "freeze", "from", "full", "grant", "group", "having", "ilike", "in",
    "initially", "inner", "intersect", "into", "is", "isnull", "join", "lateral",
    "leading", "left", "like", "limit", "localtime", "localtimestamp", "natural",
    "not", "notnull", "null", "offset", "on", "only", "or", "order", "outer",
    "overlaps", "placing", "primary", "references", "returning", "right", "select",
    "session_user", "similar", "some", "symmetric", "system_user", "table",
    "tablesample", "then", "to", "trailing", "true", "union", "unique", "user",
    "using", "variadic", "verbose", "when", "where", "window", "with",
}

TYPES = r"(uuid|text|integer|bigint|bigserial|serial|timestamptz|timestamp|date|jsonb|json|boolean|numeric|text\[\])"

problems = []
for path in sorted(pathlib.Path("db/migrations").glob("*.sql")):
    text = path.read_text()
    for m in re.finditer(rf"^\s{{2,}}([a-z_][a-z0-9_]*)\s+{TYPES}\b", text, re.M):
        if m.group(1) in RESERVED:
            line = text[: m.start()].count("\n") + 1
            problems.append(f"{path}:{line}: column '{m.group(1)}' is a reserved SQL word")
    for m in re.finditer(r"^CREATE TABLE (?:IF NOT EXISTS )?([a-z_][a-z0-9_]*)", text, re.M):
        if m.group(1) in RESERVED:
            line = text[: m.start()].count("\n") + 1
            problems.append(f"{path}:{line}: table '{m.group(1)}' is a reserved SQL word")

if problems:
    print("reserved-word check FAILED:")
    for p in problems:
        print(f"  - {p}")
    print("\nRename the identifier. Quoting works but forces every future query to quote it too.")
    sys.exit(1)
print("reserved-word check OK")
