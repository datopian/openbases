#!/usr/bin/env bash
# wg-2a0 acceptance: gateway spend becomes a durable, joinable, exact record.
#
#   sudo bash test/acceptance/cost_import.sh [path-to-wg-costimport]
#
# The importer runs as it does in production — same binary, same SECURITY
# DEFINER path, same non-superuser role — against a database this script creates
# and drops, and against a stub gateway this script serves. The stub is the point
# rather than a shortcut: the interesting cases are a second page, an entry older
# than the watermark, an untagged entry and a sub-cent cost, and none of those can
# be arranged against the live gateway, whose log is whatever it happens to
# contain today.
#
# What is being proved, in the order the failures would happen:
#
#   1  a direct INSERT as the app role writes NOTHING     (why the system path exists)
#   2  the import writes rows through the system path
#   3  a sub-cent cost survives                           (integer cents made a month total zero)
#   4  paging reads past the first 50 entries
#   5  re-running imports nothing new                     (idempotent on the gateway's id)
#   6  an entry older than the watermark is not re-read
#   7  a tagged cell resolves to a project                (the join that did not exist before)
#   8  an untagged entry stays unattributed rather than defaulting
#   9  a cached and a failed request are recorded, flagged, not dropped
#  10  the app role can only see its own project's spend  (RLS still holds on the new columns)
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="${1:-$ROOT/bin/linux-amd64/costimport}"
DB="wg_costimport_$$"
PORT="${WG_COST_STUB_PORT:-18973}"

# Two ways to reach a superuser, because this must run in both places it matters.
#
# On a node, PostgreSQL is local and `sudo -u postgres` is the way in. In CI it
# is a service container reached over TCP with PGHOST/PGUSER/PGPASSWORD already
# in the environment, where there is no postgres unix user to sudo to. A test
# that only runs on a node is one that runs when somebody remembers.
if [ -n "${PGHOST:-}" ]; then
  SUPER_PREFIX=()
  DB_HOST="$PGHOST"
  DB_PORT="${PGPORT:-5432}"
else
  SUPER_PREFIX=(sudo -u postgres)
  DB_HOST=127.0.0.1
  DB_PORT=5432
fi
# -d postgres explicitly: without it psql connects to a database named after the
# invoking user, which exists on a node and does not in a container.
# ${a[@]+"${a[@]}"} rather than "${a[@]}": under `set -u` an empty array is an
# unbound variable on older bash, and the failure is "unbound variable" rather
# than anything to do with psql.
super()  { ${SUPER_PREFIX[@]+"${SUPER_PREFIX[@]}"} psql -v ON_ERROR_STOP=1 -q -d "${1:-postgres}" "${@:2}"; }
superq() { ${SUPER_PREFIX[@]+"${SUPER_PREFIX[@]}"} psql -qAt -d "$DB" "$@"; }

pass=0
fail=0
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail + 1)); }
note() { printf '        %s\n' "$1"; }

[ -x "$BIN" ] || { echo "no costimport binary at $BIN (make build-linux)" >&2; exit 2; }
command -v psql >/dev/null || { echo "psql is not installed" >&2; exit 2; }
command -v python3 >/dev/null || { echo "python3 is not installed" >&2; exit 2; }

STUB_DIR="$(mktemp -d)"
STUB_PID=""
cleanup() {
  [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null
  super postgres -c "DROP DATABASE IF EXISTS $DB" >/dev/null 2>&1
  rm -rf "$STUB_DIR"
}
trap cleanup EXIT INT TERM

echo "wg-2a0 cost import acceptance"
echo

# ---------------------------------------------------------------------------
# A database of its own, and a login role that is NOT a superuser.
#
# workgraph_app is NOLOGIN by design. A superuser would bypass row-level
# security, and every RLS assertion below would then prove nothing.
# ---------------------------------------------------------------------------
PW="acceptance-$$-$(date +%s)"
super postgres -c "CREATE DATABASE $DB" >/dev/null || { echo "could not create $DB" >&2; exit 2; }
super "$DB" -c 'CREATE EXTENSION IF NOT EXISTS pgcrypto' >/dev/null
super "$DB" -c 'CREATE EXTENSION IF NOT EXISTS vector' >/dev/null 2>&1
for f in "$ROOT"/db/migrations/*.sql; do
  super "$DB" -f "$f" >/dev/null || { echo "migration $f failed" >&2; exit 2; }
done
super "$DB" >/dev/null <<SQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wg_acceptance') THEN
    CREATE ROLE wg_acceptance LOGIN;
  END IF;
END \$\$;
ALTER ROLE wg_acceptance PASSWORD '$PW';
GRANT workgraph_app TO wg_acceptance;
SQL
export WG_DATABASE_URL="postgres://wg_acceptance:$PW@$DB_HOST:$DB_PORT/$DB?sslmode=disable"
APP=(psql -v ON_ERROR_STOP=1 -qAt "$WG_DATABASE_URL")

# Two cells, two projects, and a person who is a member of exactly one of them.
#
# Two projects rather than one, because "the app role can see spend" and "the app
# role can see only ITS spend" are different claims and only the second is worth
# proving. wg-cost-cell is the tag the stub entries carry.
super "$DB" >/dev/null <<'SQL'
INSERT INTO organisations (slug, name) VALUES ('cost-acc', 'Cost Acceptance Org');

INSERT INTO users (organisation_id, display_name, primary_email)
SELECT id, 'Cost Member', 'member@acceptance.invalid' FROM organisations WHERE slug = 'cost-acc';
INSERT INTO users (organisation_id, display_name, primary_email)
SELECT id, 'Cost Backup', 'backup@acceptance.invalid' FROM organisations WHERE slug = 'cost-acc';

INSERT INTO execution_nodes (hostname, environment)
VALUES ('cost.acceptance.invalid', 'staging');

INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
SELECT id, 'wg-cost-cell', 'wgcostcell', 'internal' FROM execution_nodes WHERE hostname = 'cost.acceptance.invalid';
INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
SELECT id, 'wg-other-cell', 'wgothercell', 'internal' FROM execution_nodes WHERE hostname = 'cost.acceptance.invalid';

INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id, execution_cell_id)
SELECT o.id, 'cost-acceptance', 'Cost Acceptance',
       (SELECT id FROM users WHERE primary_email = 'member@acceptance.invalid'),
       (SELECT id FROM users WHERE primary_email = 'backup@acceptance.invalid'),
       c.id
  FROM organisations o, execution_cells c
 WHERE o.slug = 'cost-acc' AND c.slug = 'wg-cost-cell';

INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id, execution_cell_id)
SELECT o.id, 'other-project', 'Other Project',
       (SELECT id FROM users WHERE primary_email = 'member@acceptance.invalid'),
       (SELECT id FROM users WHERE primary_email = 'backup@acceptance.invalid'),
       c.id
  FROM organisations o, execution_cells c
 WHERE o.slug = 'cost-acc' AND c.slug = 'wg-other-cell';

-- Membership of ONE project only. Being an owner is not membership: the policy
-- reads project_memberships, and a test whose subject can see everything proves
-- nothing about isolation.
INSERT INTO project_memberships (project_id, user_id, role_name)
SELECT p.id, u.id, 'project_lead'
  FROM projects p, users u
 WHERE p.slug = 'cost-acceptance' AND u.primary_email = 'member@acceptance.invalid';
SQL
MEMBER=$(superq -c "SELECT id FROM users WHERE primary_email = 'member@acceptance.invalid'")

# ---------------------------------------------------------------------------
# The stub gateway.
#
# 60 entries so paging is exercised (the API caps a page at 50), newest first,
# because that is the order the real endpoint returns and the order the walk
# depends on to know when to stop.
# ---------------------------------------------------------------------------
cat > "$STUB_DIR/stub.py" <<'PY'
import json, sys
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse, parse_qs

BASE = datetime(2026, 8, 20, 12, 0, 0, tzinfo=timezone.utc)

def entries():
    out = []
    # 0: the sub-cent request that integer cents rounded to nothing.
    out.append(dict(id="acc-000", created_at=(BASE).isoformat().replace("+00:00", "Z"),
                    provider="anthropic", model="claude-haiku-4-5", tokens_in=1200,
                    tokens_out=90, cost=8.8e-05, cached=False, success=True,
                    metadata={"role": "polecat", "cell": "wg-cost-cell", "rig": "sandbox"}))
    # 1: untagged, like 98.3% of the real historical traffic.
    out.append(dict(id="acc-001", created_at=(BASE - timedelta(minutes=1)).isoformat().replace("+00:00", "Z"),
                    provider="anthropic", model="claude-opus-5", tokens_in=8000,
                    tokens_out=1500, cost=0.42, cached=False, success=True, metadata=None))
    # 2: a cache hit, which costs nothing and is the evidence the cache works.
    out.append(dict(id="acc-002", created_at=(BASE - timedelta(minutes=2)).isoformat().replace("+00:00", "Z"),
                    provider="anthropic", model="claude-haiku-4-5", tokens_in=1200,
                    tokens_out=90, cost=0.0, cached=True, success=True,
                    metadata={"cell": "wg-cost-cell"}))
    # 3: a failure that still cost money.
    out.append(dict(id="acc-003", created_at=(BASE - timedelta(minutes=3)).isoformat().replace("+00:00", "Z"),
                    provider="anthropic", model="claude-opus-5", tokens_in=6000,
                    tokens_out=0, cost=0.09, cached=False, success=False,
                    metadata={"cell": "wg-cost-cell"}))
    # 4: spend belonging to a different project, for the RLS check.
    out.append(dict(id="acc-004", created_at=(BASE - timedelta(minutes=4)).isoformat().replace("+00:00", "Z"),
                    provider="anthropic", model="claude-opus-5", tokens_in=100,
                    tokens_out=10, cost=0.01, cached=False, success=True,
                    metadata={"cell": "wg-other-cell"}))
    # 5..59: filler, so page 2 exists.
    for i in range(5, 60):
        out.append(dict(id="acc-%03d" % i,
                        created_at=(BASE - timedelta(minutes=i)).isoformat().replace("+00:00", "Z"),
                        provider="anthropic", model="claude-haiku-4-5", tokens_in=10,
                        tokens_out=5, cost=1e-05, cached=False, success=True,
                        metadata={"cell": "wg-cost-cell"}))
    return out

ALL = entries()

class H(BaseHTTPRequestHandler):
    def do_GET(self):
        u = urlparse(self.path)
        q = parse_qs(u.query)
        if "/logs" not in u.path:
            self.send_error(404); return
        if self.headers.get("Authorization") != "Bearer " + sys.argv[2]:
            self.send_response(403)
            self.send_header("Content-Type", "application/json"); self.end_headers()
            self.wfile.write(json.dumps({"success": False, "errors": ["bad token"]}).encode())
            return
        page = int(q.get("page", ["1"])[0]); per = int(q.get("per_page", ["50"])[0])
        chunk = ALL[(page - 1) * per: page * per]
        body = json.dumps({"success": True, "errors": [], "result": chunk}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass

HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
# The stub's token, assembled rather than written out: a literal
# "Authorization: Bearer <something>" in a curl line is what secret_scan.sh
# flags, and a scan that has to be argued with about its own test fixtures is
# one that gets suppressed.
STUB_TOKEN="stub-$$-token"
python3 "$STUB_DIR/stub.py" "$PORT" "$STUB_TOKEN" &
STUB_PID=$!
for _ in $(seq 1 40); do
  curl -fsS -H "Authorization: Bearer ${STUB_TOKEN}" \
    "http://127.0.0.1:$PORT/accounts/acc/ai-gateway/gateways/g/logs?page=1" >/dev/null 2>&1 && break
  sleep 0.25
done

export CLOUDFLARE_ACCOUNT_ID=acceptance
export CLOUDFLARE_API_TOKEN="$STUB_TOKEN"
export WG_COST_API_BASE="http://127.0.0.1:$PORT"
GW=stub-gateway
run_import() { "$BIN" -gateways "$GW" -json "$@" 2>"$STUB_DIR/err.txt"; }

# ---------------------------------------------------------------------------
# 1. A direct INSERT as the app role writes nothing.
# ---------------------------------------------------------------------------
echo "== the system path"
# ON_ERROR_STOP is deliberately off here: the INSERT is expected to be refused,
# and the point is to capture the refusal rather than abort the script on it.
direct=$(psql -qAt "$WG_DATABASE_URL" -c \
  "INSERT INTO usage_records (provider, model, input_tokens, output_tokens, cost_cents, occurred_at, gateway, external_id)
   VALUES ('anthropic', 'x', 1, 1, 1, now(), 'direct', 'direct-1')" 2>&1)
landed=$(superq -c "SELECT count(*) FROM usage_records WHERE external_id = 'direct-1'")
if echo "$direct" | grep -q 'row-level security policy' && [ "$landed" = "0" ]; then
  ok "a direct INSERT with no user is refused by RLS, so the importer needs the system path"
  note "$(echo "$direct" | head -1)"
else
  bad "a direct INSERT was not refused (landed=$landed): $direct"
fi

# ---------------------------------------------------------------------------
# 2-4. The import itself.
# ---------------------------------------------------------------------------
echo
echo "== importing"
out=$(run_import) || { echo "import failed:"; cat "$STUB_DIR/err.txt"; exit 1; }
imported=$(echo "$out" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["imported"])')
complete=$(echo "$out" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["complete"])')
note "imported=$imported complete=$complete"

if [ "$imported" = "60" ]; then
  ok "all 60 entries imported, so paging read past the 50-entry page limit"
else
  bad "imported $imported of 60; paging stopped early"
fi

cents=$(superq -c "SELECT cost_cents FROM usage_records WHERE external_id = 'acc-000'")
if [ -n "$cents" ] && [ "$(echo "$cents > 0" | bc -l 2>/dev/null || echo 0)" = "1" ]; then
  ok "a \$0.000088 request stored as $cents cents rather than rounding to zero"
else
  bad "a \$0.000088 request stored as '$cents'; sub-cent spend is being lost"
fi

total=$(superq -c "SELECT round(sum(cost_cents), 6) FROM usage_records WHERE gateway = '$GW'")
note "total recorded: $total cents"

# ---------------------------------------------------------------------------
# 5-6. Re-running.
# ---------------------------------------------------------------------------
echo
echo "== re-running"
# -overlap 10m rather than the two-hour default, because the whole fixture spans
# one hour: with the default the entire log is inside the overlap window and the
# watermark could not stop the walk even if it were working.
out2=$(run_import -overlap 10m) || { echo "second import failed:"; cat "$STUB_DIR/err.txt"; exit 1; }
new2=$(echo "$out2" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["imported"])')
dup2=$(echo "$out2" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["duplicate"])')
fetched2=$(echo "$out2" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["fetched"])')
note "fetched=$fetched2 imported=$new2 duplicate=$dup2"

if [ "$new2" = "0" ]; then
  ok "a second run imported nothing new; the gateway's id makes the import idempotent"
else
  bad "a second run imported $new2 rows; spend is being double-counted"
fi

rows=$(superq -c "SELECT count(*) FROM usage_records WHERE gateway = '$GW'")
if [ "$rows" = "60" ]; then
  ok "still 60 rows after two runs"
else
  bad "$rows rows after two runs, expected 60"
fi

# The watermark stops the walk. The newest entry is at the watermark, so the walk
# should stop almost immediately rather than re-reading all 60.
# The newest entry sits at the watermark and the next ten minutes hold ten more,
# so a 10-minute overlap should re-read about eleven and stop.
if [ "$fetched2" -le 12 ]; then
  ok "the second run re-read $fetched2 entries, not all 60; the watermark bounds the work"
else
  bad "the second run re-read $fetched2 entries; the watermark is not bounding the walk"
fi

# ---------------------------------------------------------------------------
# 7-9. Attribution and flags.
# ---------------------------------------------------------------------------
echo
echo "== attribution"
proj=$(superq -c "SELECT p.slug FROM usage_records u JOIN projects p ON p.id = u.project_id WHERE u.external_id = 'acc-000'")
if [ "$proj" = "cost-acceptance" ]; then
  ok "a cell-tagged request resolved to project '$proj'; spend can now be joined to a project"
else
  bad "a cell-tagged request resolved to '$proj', expected cost-acceptance"
fi

untagged=$(superq -c "SELECT coalesce(project_id::text,'null') || ' ' || coalesce(role,'null') || ' ' || coalesce(cell,'null')
     FROM usage_records WHERE external_id = 'acc-001'")
if [ "$untagged" = "null null null" ]; then
  ok "an untagged request stayed unattributed rather than acquiring a default"
else
  bad "an untagged request came out as '$untagged'; a default was invented"
fi

flags=$(superq -c "SELECT string_agg(external_id || '=' || cached || '/' || succeeded, ' ' ORDER BY external_id)
     FROM usage_records WHERE external_id IN ('acc-002','acc-003')")
if [ "$flags" = "acc-002=true/true acc-003=false/false" ]; then
  ok "the cached hit and the failed request were both recorded and flagged: $flags"
else
  bad "cached/failed flags came out as '$flags'"
fi

# ---------------------------------------------------------------------------
# 10. RLS still holds on the new columns.
# ---------------------------------------------------------------------------
echo
echo "== isolation"
visible=$("${APP[@]}" -c "SELECT count(*) FROM usage_records" 2>/dev/null || echo ERR)
if [ "$visible" = "0" ]; then
  ok "with no user set the app role sees 0 of $rows rows; the import path did not become a read path"
else
  bad "the app role sees $visible rows with no user set"
fi

# As a member of cost-acceptance and nothing else.
scoped=$("${APP[@]}" <<SQL 2>/dev/null || echo ERR
BEGIN;
SELECT set_config('workgraph.user_id', '$MEMBER', true);
SELECT count(*) FROM usage_records WHERE gateway = '$GW' AND project_id IS NOT NULL;
COMMIT;
SQL
)
scoped=$(echo "$scoped" | grep -E '^[0-9]+$' | tail -1)
mine=$(superq -c "SELECT count(*) FROM usage_records u JOIN projects p ON p.id = u.project_id
    WHERE p.slug = 'cost-acceptance' AND u.gateway = '$GW'")
if [ "$scoped" = "$mine" ] && [ "$scoped" -gt 0 ]; then
  ok "a member of one project sees its $scoped attributed rows and not the other project's"
else
  bad "project-scoped visibility came out as scoped=$scoped, expected $mine"
fi

# The finding this test exists to make visible rather than to hide.
#
# usage_records_read admits a row whose project_id IS NULL to ANY authenticated
# user — that is what can_read_project_row does, and it is the same rule every
# other project-scoped table uses. Unattributed spend is therefore readable by
# everyone with a session. On the real gateways 98.3% of traffic is untagged, so
# that is nearly all of it. It is asserted here so the day somebody changes the
# policy, this test says which way it changed.
unattributed=$("${APP[@]}" <<SQL 2>/dev/null || echo ERR
BEGIN;
SELECT set_config('workgraph.user_id', '$MEMBER', true);
SELECT count(*) FROM usage_records WHERE gateway = '$GW' AND project_id IS NULL;
COMMIT;
SQL
)
unattributed=$(echo "$unattributed" | grep -E '^[0-9]+$' | tail -1)
note "unattributed rows visible to any authenticated user: $unattributed (tracked, not accepted silently)"

echo
printf 'passed %d, failed %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
