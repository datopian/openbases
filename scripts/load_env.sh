#!/usr/bin/env bash
# Create, seed, or drop the WP-I3 load-test database on the staging control node.
#
#   scripts/load_env.sh create   # database + migrations + seed to the load target
#   scripts/load_env.sh status   # row counts and size
#   scripts/load_env.sh drop     # remove it entirely
#
# A SEPARATE database, deliberately. The load target is 50 projects and 105,000
# work items; the pilot database has 3 projects, and
# test/integration/pilot_registry.sql asserts exactly that. Seeding the load data
# where the pilot lives would break a test that is doing its job, and would leave
# the staging UI showing fifty fictional projects.
#
# It shares the PostgreSQL instance on purpose. Criterion 3 of WP-I3 is that
# agent workloads cannot take down the control plane, and a load test on a
# different server would not exercise that at all.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ACTION="${1:?usage: load_env.sh <create|seed|status|drop>}"
DB="${WG_LOAD_DB:-workgraph_load}"
HOST="${WG_CONTROL_HOST:-ssh-staging.openbases.com}"

SSH=(ssh -o ProxyCommand="cloudflared access ssh --hostname %h"
     -o StrictHostKeyChecking=no -o ConnectTimeout=30 "root@${HOST}")

psql_as_postgres() {  # reads SQL on stdin
  "${SSH[@]}" "cat > /tmp/wg-load.sql && chown postgres /tmp/wg-load.sql && \
    su - postgres -c 'psql -v ON_ERROR_STOP=1 -q -d ${DB} -f /tmp/wg-load.sql'; \
    rc=\$?; rm -f /tmp/wg-load.sql; exit \$rc"
}

case "$ACTION" in
create)
  echo "== creating ${DB}"
  "${SSH[@]}" "su - postgres -c \"psql -q -c 'CREATE DATABASE ${DB}'\" 2>&1 | grep -v 'already exists' || true"
  # The application role and the RLS helpers come from the migrations, so the
  # schema is built the same way production is rather than copied.
  "${SSH[@]}" "su - postgres -c \"psql -q -d ${DB} -c 'CREATE EXTENSION IF NOT EXISTS pgcrypto'\" 2>&1 | tail -1"
  "${SSH[@]}" "su - postgres -c \"psql -q -d ${DB} -c 'CREATE EXTENSION IF NOT EXISTS vector'\" 2>&1 | tail -1"

  echo "== applying migrations"
  # The migrate binary embeds them, so this is the same set the pilot database
  # has, in the same order, with the same checksums.
  "${SSH[@]}" "test -x /var/lib/postgresql/wg-migrate" 2>/dev/null || {
    echo "   upload a linux migrate binary first:" >&2
    echo "   GOOS=linux GOARCH=amd64 go build -o /tmp/m ./cmd/migrate && \\" >&2
    echo "   scp /tmp/m root@${HOST}:/var/lib/postgresql/wg-migrate" >&2
    exit 1
  }
  "${SSH[@]}" "chown postgres /var/lib/postgresql/wg-migrate; chmod 755 /var/lib/postgresql/wg-migrate; \
    su - postgres -c './wg-migrate -dsn \"postgres:///${DB}?host=/var/run/postgresql\"' 2>&1 | tail -3"
  echo "== seeding — run 'load_env.sh seed' next"
  ;;

seed)
  echo "== seeding ${DB} to the load target (this inserts ~105,000 rows)"
  psql_as_postgres < "$ROOT/db/migrations/0009_pilot_seed.sql" >/dev/null 2>&1 || true
  psql_as_postgres < "$ROOT/test/load/seed.sql" 2>&1 | tail -8
  ;;

status)
  psql_as_postgres <<'EOF' 2>&1 | tail -14
SELECT
  (SELECT count(*) FROM projects)                                     AS projects,
  (SELECT count(*) FROM work_refs)                                    AS work_items,
  (SELECT count(*) FROM work_refs WHERE status = 'open')              AS open_items,
  (SELECT count(*) FROM users)                                        AS users,
  (SELECT count(*) FROM project_memberships)                          AS memberships,
  pg_size_pretty(pg_database_size(current_database()))                 AS size;
EOF
  ;;

drop)
  echo "== dropping ${DB}"
  "${SSH[@]}" "su - postgres -c \"psql -q -c 'DROP DATABASE IF EXISTS ${DB}'\" 2>&1 | tail -1"
  ;;

*) echo "unknown action: $ACTION" >&2; exit 2 ;;
esac
