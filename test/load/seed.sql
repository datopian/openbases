-- Seed a database to the WP-I3 load target.
--
--   50 projects, 100,000 historical work items, 5,000 open work items
--
-- Run against a SEPARATE database, never the pilot one. test/integration/
-- pilot_registry.sql asserts exactly three projects and twelve repositories, so
-- seeding fifty into the same database would break a test that is doing its job.
-- scripts/load_env.sh creates workgraph_load for this.
--
-- The shape matters more than the volume. Row-level security is enforced through
-- project membership, so a leak test needs many projects, users who are members
-- of some and not others, and work items spread across all of them. A hundred
-- thousand rows all visible to everyone would load the planner and prove
-- nothing about isolation.
--
-- Written with generate_series rather than a client-side loop: one statement,
-- one transaction, and the planner sees the real distribution.

BEGIN;

-- Reuse the seeded organisation so foreign keys and the existing roles apply.
CREATE TEMP TABLE ctx AS
SELECT (SELECT id FROM organisations ORDER BY created_at LIMIT 1) AS org_id;

-- ---------------------------------------------------------------------------
-- An execution node and two cells
-- ---------------------------------------------------------------------------
-- The pilot seed does not create these — cells come from the registry work, not
-- from a migration — so a fresh load database has none, and a restricted
-- project cannot exist without one (CONSTRAINT restricted_requires_cell). Found
-- by the constraint doing its job on the first seed attempt.
--
-- Two cells, not one, because the point of a restricted project is that it runs
-- somewhere its own: a leak test where every project shares a cell would not
-- represent the deployment.
INSERT INTO execution_nodes (hostname, environment)
VALUES ('load-execution.invalid', 'staging')
ON CONFLICT (hostname) DO NOTHING;

INSERT INTO execution_cells
    (execution_node_id, slug, system_username, trust_domain,
     max_concurrent_agents, cpu_quota_percent, memory_limit_mb)
SELECT n.id, v.slug, v.username, v.domain, 2, 200, 8192
  FROM execution_nodes n
  JOIN (VALUES
      ('load-oss',    'wgload_oss',    'oss'),
      ('load-client', 'wgload_client', 'client-load')
   ) AS v(slug, username, domain) ON true
 WHERE n.hostname = 'load-execution.invalid'
ON CONFLICT (slug) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Users: 60, so that membership can be genuinely partial
-- ---------------------------------------------------------------------------
INSERT INTO users (organisation_id, display_name, primary_email)
SELECT ctx.org_id, 'Load User ' || n, 'load-user-' || n || '@example.invalid'
  FROM ctx, generate_series(1, 60) AS n
ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------
-- 50 projects, with a deliberate visibility mix
-- ---------------------------------------------------------------------------
-- Every fifth project is restricted, which is the case that must never leak.
-- A restricted project needs its own execution cell, so those reuse the
-- existing client cell rather than inventing one.
INSERT INTO projects
    (organisation_id, slug, name, visibility, primary_owner_id, backup_owner_id,
     execution_cell_id, status)
SELECT ctx.org_id,
       'load-project-' || n,
       'Load Project ' || n,
       CASE WHEN n % 5 = 0 THEN 'restricted'
            WHEN n % 3 = 0 THEN 'confidential'
            ELSE 'internal' END,
       (SELECT id FROM users WHERE primary_email = 'load-user-' || (1 + (n % 60)) || '@example.invalid'),
       (SELECT id FROM users WHERE primary_email = 'load-user-' || (1 + ((n + 7) % 60)) || '@example.invalid'),
       CASE WHEN n % 5 = 0
            THEN (SELECT id FROM execution_cells WHERE slug = 'load-client')
            ELSE NULL END,
       'active'
  FROM ctx, generate_series(1, 50) AS n
ON CONFLICT (organisation_id, slug) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Membership: partial on purpose
-- ---------------------------------------------------------------------------
-- Each project gets three members drawn by a stride, so a given user is a
-- member of a few projects and a non-member of most. That asymmetry is what the
-- leak test measures against.
INSERT INTO project_memberships (project_id, user_id, role_name)
SELECT p.id,
       (SELECT id FROM users WHERE primary_email = 'load-user-' || (1 + ((n * 7 + k) % 60)) || '@example.invalid'),
       CASE k WHEN 0 THEN 'project_lead' WHEN 1 THEN 'contributor' ELSE 'observer' END
  FROM generate_series(1, 50) AS n
  JOIN projects p ON p.slug = 'load-project-' || n
  CROSS JOIN generate_series(0, 2) AS k
ON CONFLICT DO NOTHING;

-- The owners must be members too, which the pilot seed also asserts.
INSERT INTO project_memberships (project_id, user_id, role_name)
SELECT p.id, p.primary_owner_id, 'project_lead'
  FROM projects p WHERE p.slug LIKE 'load-project-%'
ON CONFLICT DO NOTHING;
INSERT INTO project_memberships (project_id, user_id, role_name)
SELECT p.id, p.backup_owner_id, 'backup_operator'
  FROM projects p WHERE p.slug LIKE 'load-project-%'
ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------
-- A Beads database per project, since work_refs requires one
-- ---------------------------------------------------------------------------
INSERT INTO beads_databases (organisation_id, execution_cell_id, name, scope)
SELECT ctx.org_id,
       (SELECT id FROM execution_cells WHERE slug = 'load-oss'),
       'load-graph-' || n,
       'project'
  FROM ctx, generate_series(1, 50) AS n
ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------
-- 105,000 work items: 100,000 closed, 5,000 open
-- ---------------------------------------------------------------------------
-- Visibility is inherited from the project rather than chosen independently, so
-- a restricted project's work items are restricted — which is what makes a leak
-- of one meaningful.
INSERT INTO work_refs
    (organisation_id, beads_database_id, bead_id, title, kind, status,
     visibility, project_id, last_seen_at)
SELECT ctx.org_id,
       b.id,
       'load-' || n || '-' || i,
       'Historical work item ' || i || ' in project ' || n,
       CASE i % 4 WHEN 0 THEN 'bug' WHEN 1 THEN 'task' WHEN 2 THEN 'chore' ELSE 'epic' END,
       'closed',
       p.visibility,
       p.id,
       now() - (i || ' minutes')::interval
  FROM ctx,
       generate_series(1, 50) AS n
  JOIN projects p ON p.slug = 'load-project-' || n
  JOIN beads_databases b ON b.name = 'load-graph-' || n
  CROSS JOIN generate_series(1, 2000) AS i
ON CONFLICT DO NOTHING;

INSERT INTO work_refs
    (organisation_id, beads_database_id, bead_id, title, kind, status,
     visibility, project_id, last_seen_at)
SELECT ctx.org_id,
       b.id,
       'load-open-' || n || '-' || i,
       'Open work item ' || i || ' in project ' || n,
       CASE i % 3 WHEN 0 THEN 'bug' WHEN 1 THEN 'task' ELSE 'chore' END,
       'open',
       p.visibility,
       p.id,
       now() - (i || ' minutes')::interval
  FROM ctx,
       generate_series(1, 50) AS n
  JOIN projects p ON p.slug = 'load-project-' || n
  JOIN beads_databases b ON b.name = 'load-graph-' || n
  CROSS JOIN generate_series(1, 100) AS i
ON CONFLICT DO NOTHING;

COMMIT;

-- Indexes the UI actually needs at this volume, built AFTER the bulk insert.
--
-- work_refs has no index on project_id in the base schema, which is fine at
-- three projects and not fine at fifty: every project-scoped query becomes a
-- sequential scan of 105,000 rows, and the leak test — which asks "which
-- projects can this user see" sixty times — took over ten minutes before this
-- existed. Filed as a finding; a real deployment needs it.
CREATE INDEX IF NOT EXISTS work_refs_project_status
    ON work_refs (project_id, status);
CREATE INDEX IF NOT EXISTS project_memberships_user
    ON project_memberships (user_id, project_id);

-- Indexes are built AFTER the bulk insert. Inserting 105,000 rows into an
-- indexed table costs several times as much as inserting then indexing, and the
-- point of this file is to reach the target quickly enough that anyone will
-- actually run it.
ANALYZE work_refs;
ANALYZE projects;
ANALYZE project_memberships;

SELECT
  (SELECT count(*) FROM projects WHERE slug LIKE 'load-project-%')      AS projects,
  (SELECT count(*) FROM work_refs WHERE bead_id LIKE 'load-%')          AS work_items,
  (SELECT count(*) FROM work_refs WHERE bead_id LIKE 'load-open-%')     AS open_items,
  (SELECT count(*) FROM users WHERE primary_email LIKE 'load-user-%')   AS users,
  (SELECT count(*) FROM project_memberships m
     JOIN projects p ON p.id = m.project_id
    WHERE p.slug LIKE 'load-project-%')                                 AS memberships;
