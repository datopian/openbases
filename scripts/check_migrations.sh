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
  #
  # Statements inside a $$-quoted function body are excluded: they are the
  # definition of code that runs later, not a change this migration makes. A
  # marker there would assert a backfill that never happens, which is worse than
  # no marker at all — it trains the reader to ignore the marker.
  if awk '/\$\$/{q=!q} !q{print}' "$f" | grep -qiE '^\s*(UPDATE|DELETE)\s+' && ! grep -q 'wg:backfill' "$f"; then
    note "$base: data modification without a 'wg:backfill' marker"
  fi
done

# Duplicate sequence numbers would apply in an ambiguous order.
dupes="$(printf '%s\n' "${files[@]}" | xargs -n1 basename 2>/dev/null | cut -c1-4 | sort | uniq -d)"
[ -n "$dupes" ] && note "duplicate migration numbers: $dupes"

# A view over a protected table must be evaluated as the CALLER.
#
# Since PostgreSQL 15 a view runs with the permissions of its OWNER unless it
# declares security_invoker = true. These migrations are applied by a superuser,
# and a superuser never evaluates row-level security at all — so an ordinary view
# over a protected table hands every row to anyone who can select from the view,
# while the table underneath looks correctly protected.
#
# This is not hypothetical: credential_rotation_due was exactly that, and a
# non-admin could read the whole credential registry through it (fixed in 0022).
# The hole sat four lines below the policy it defeated.
for f in "${files[@]}"; do
  base="$(basename "$f")"
  # Views declared and views hardened may be in different migrations, so the
  # check is over the whole set rather than per file.
  awk '/CREATE (OR REPLACE )?VIEW/ {
        name = $0
        sub(/.*VIEW[[:space:]]+/, "", name)
        sub(/[[:space:]].*$/, "", name)
        sub(/\(.*$/, "", name)
        print name
      }' "$f"
done | sort -u | while read -r view; do
  [ -n "$view" ] || continue
  if ! grep -rqE "ALTER VIEW[[:space:]]+$view[[:space:]]+SET[[:space:]]*\([[:space:]]*security_invoker" "${files[@]}" \
     && ! grep -rqE "CREATE (OR REPLACE )?VIEW[[:space:]]+$view[[:space:]]+WITH[[:space:]]*\([^)]*security_invoker" "${files[@]}"; then
    note "view $view never sets security_invoker; it will be evaluated as its owner and bypass row-level security"
  fi
done

# Every SQL integration test must actually run.
#
# A test file that exists but is not referenced by the workflow passes silently
# forever and reads as coverage in review. This happened: the projection test was
# added on a branch cut before the step it anchored to existed, so the edit was a
# no-op and CI went green having never run it.
for t in test/integration/*.sql; do
  [ -e "$t" ] || continue
  # The FULL path, not the basename. A comment mentioning 0007_projections.sql
  # matches a bare "projections.sql" and would satisfy this check without any
  # step existing — the same false positive that hid the gap in the first place.
  if grep -qF "$t" .github/workflows/ci.yml; then
    continue
  fi
  # A file may also earn its place by being INCLUDED by a test that CI runs,
  # rather than being a test itself — assert_app_role.sql is machinery shared by
  # every test that drops to workgraph_app. Requiring a \ir reference from a
  # file the workflow actually runs keeps the original guarantee: the file is
  # reached on every CI run, or it is reported.
  inc="$(basename "$t")"
  if grep -rqE "^\\\\ir[[:space:]]+$inc\\b" test/integration/*.sql 2>/dev/null; then
    # And whatever includes it must itself be run, or this is circular.
    included_by_live=0
    for u in $(grep -rlE "^\\\\ir[[:space:]]+$inc\\b" test/integration/*.sql); do
      grep -qF "$u" .github/workflows/ci.yml && included_by_live=1
    done
    if [ "$included_by_live" = 1 ]; then
      continue
    fi
    note "$inc is only included by tests that CI never runs"
    continue
  fi
  note "$(basename "$t") exists but is never run by .github/workflows/ci.yml"
done

python3 scripts/check_reserved_words.py || fail=1

if [ "$fail" -eq 0 ]; then echo "migration checks OK"; else echo "migration checks FAILED"; fi
exit "$fail"
