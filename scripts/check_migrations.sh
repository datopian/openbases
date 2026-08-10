#!/usr/bin/env bash
# Structural checks on SQL migrations.
#
# These are cheap guards against the mistakes that are expensive in production.
# Applying migrations against a real database is done by CI with a live
# PostgreSQL service (WP-C1).
set -uo pipefail

cd "$(dirname "$0")/.."
DIR=db/migrations
fail=0
note() { printf '  - %s\n' "$1"; fail=1; }

shopt -s nullglob
files=("$DIR"/*.sql)
echo "migration checks (${#files[@]} file(s))"

for f in "${files[@]}"; do
  base="$(basename "$f")"

  # Numbered, ordered, and unique.
  if ! [[ "$base" =~ ^[0-9]{4}_[a-z0-9_]+\.sql$ ]]; then
    note "$base: name must match NNNN_lower_snake.sql"
  fi

  # Every migration is transactional, so a partial apply cannot leave a
  # half-migrated schema behind.
  grep -qiE '^\s*BEGIN\s*;' "$f" || note "$base: missing BEGIN;"
  grep -qiE '^\s*COMMIT\s*;' "$f" || note "$base: missing COMMIT;"

  # Destructive operations are protected actions and need an explicit,
  # reviewed marker so they cannot arrive unnoticed in a routine migration.
  if grep -qiE '^\s*(DROP\s+(TABLE|COLUMN)|ALTER\s+TABLE\s+\S+\s+DROP\s+COLUMN|TRUNCATE)' "$f"; then
    grep -q 'wg:destructive-approved' "$f" || \
      note "$base: destructive statement without a 'wg:destructive-approved' marker and approval reference"
  fi

  # Migrations change schema; they do not edit business records.
  if grep -qiE '^\s*(UPDATE|DELETE)\s+' "$f" && ! grep -q 'wg:backfill' "$f"; then
    note "$base: data modification without a 'wg:backfill' marker"
  fi
done

# Duplicate sequence numbers would apply in an ambiguous order.
dupes="$(printf '%s\n' "${files[@]}" | xargs -n1 basename 2>/dev/null | cut -c1-4 | sort | uniq -d)"
[ -n "$dupes" ] && note "duplicate migration numbers: $dupes"

if [ "$fail" -eq 0 ]; then echo "migration checks OK"; else echo "migration checks FAILED"; fi
exit "$fail"
