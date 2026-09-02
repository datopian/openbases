-- The work overview shows only what the caller may see (B1).
--
-- system_work_overview and system_queue_overview are SECURITY DEFINER, which
-- is right -- they join RLS-protected work_refs and usage_records against
-- work_queue, which is platform state with no project of its own -- and
-- neither filtered by anything. 0036's own comment says "the visibility rule
-- is written once, here", and no rule was written, so GET /v1/work returned
-- every bead in every project to any authenticated caller.
--
-- Both directions are asserted. A filter that returns nothing to everybody
-- would pass a leak test and break the product.

BEGIN;

DO $$
DECLARE
  org uuid; node uuid; cell uuid;
  alpha uuid; beta uuid;
  member uuid; outsider uuid; boss uuid;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Overview member', 'test-ov-member@example.invalid') RETURNING id INTO member;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Overview outsider', 'test-ov-outsider@example.invalid') RETURNING id INTO outsider;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Overview manager', 'test-ov-manager@example.invalid') RETURNING id INTO boss;

  -- A company-wide grant, so the management branch is exercised by somebody
  -- who is not a member of either project.
  INSERT INTO role_grants (user_id, role_name, organisation_id)
  VALUES (boss, 'organisation_admin', org);

  PERFORM set_config('test.member', member::text, false);
  PERFORM set_config('test.outsider', outsider::text, false);
  PERFORM set_config('test.boss', boss::text, false);

  INSERT INTO execution_nodes (hostname, environment) VALUES ('test-ov-node', 'staging');
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  SELECT id, 'test-ov-cell', 'wgcell_test_ov', 'client-restricted'
    FROM execution_nodes WHERE hostname = 'test-ov-node' RETURNING id INTO cell;

  INSERT INTO projects (organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id, execution_cell_id)
  VALUES (org, 'test-ov-alpha', 'Overview alpha', 'confidential', member, outsider, NULL)
  RETURNING id INTO alpha;
  INSERT INTO projects (organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id, execution_cell_id)
  VALUES (org, 'test-ov-beta', 'Overview beta', 'restricted', outsider, member, cell)
  RETURNING id INTO beta;

  -- The member belongs to alpha only. Ownership is not readership.
  INSERT INTO project_memberships (project_id, user_id, role_name)
  VALUES (alpha, member, 'contributor');

  INSERT INTO beads_databases (organisation_id, name, scope, project_id, path, host)
  VALUES (org, 'test-ov-alpha-graph', 'project', alpha, '/srv/graphs/test-ov-alpha', 'test-control'),
         (org, 'test-ov-beta-graph', 'project', beta, '/srv/graphs/test-ov-beta', 'test-control');

  INSERT INTO work_refs (organisation_id, beads_database_id, bead_id, title, kind,
                         status, visibility, project_id, execution_cell_id)
  SELECT org, b.id, 'ova-1', 'Alpha work', 'task', 'open', 'confidential', alpha, cell
    FROM beads_databases b WHERE b.name = 'test-ov-alpha-graph';
  INSERT INTO work_refs (organisation_id, beads_database_id, bead_id, title, kind,
                         status, visibility, project_id, execution_cell_id)
  SELECT org, b.id, 'ovb-1', 'Beta work', 'task', 'open', 'restricted', beta, cell
    FROM beads_databases b WHERE b.name = 'test-ov-beta-graph';
  -- Company-wide work, belonging to no project.
  INSERT INTO work_refs (organisation_id, beads_database_id, bead_id, title, kind,
                         status, visibility, project_id, execution_cell_id)
  SELECT org, b.id, 'ovc-1', 'Company work', 'task', 'open', 'internal', NULL, cell
    FROM beads_databases b WHERE b.name = 'test-ov-alpha-graph';

  -- One work job per bead, and a plan job the outsider requested. A brief is
  -- the text somebody pasted in; for a client engagement that text IS the
  -- engagement, which is why a plan job is not company-readable.
  INSERT INTO work_queue (kind, cell, rig, bead, status, requested_by)
  VALUES ('work', 'test-ov-cell', 'sandbox', 'ova-1', 'queued', member),
         ('work', 'test-ov-cell', 'sandbox', 'ovb-1', 'queued', outsider);
  INSERT INTO work_queue (kind, cell, rig, brief, status, requested_by)
  VALUES ('plan', 'test-ov-cell', 'sandbox', 'A brief only its author should see', 'queued', outsider);
END $$;

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE n integer;
BEGIN
  -- The member: alpha and the company bead, never beta.
  PERFORM set_config('workgraph.user_id', current_setting('test.member'), true);

  SELECT count(*) INTO n FROM system_work_overview(NULL) WHERE bead = 'ova-1';
  IF n <> 1 THEN RAISE EXCEPTION 'a member cannot see their own project''s bead'; END IF;
  SELECT count(*) INTO n FROM system_work_overview(NULL) WHERE bead = 'ovc-1';
  IF n <> 1 THEN RAISE EXCEPTION 'a member cannot see company-wide work'; END IF;
  SELECT count(*) INTO n FROM system_work_overview(NULL) WHERE bead = 'ovb-1';
  IF n <> 0 THEN RAISE EXCEPTION 'a member sees a bead from a project they are not in'; END IF;

  -- And through the cell filter too, which is the argument the UI passes.
  SELECT count(*) INTO n FROM system_work_overview('test-ov-cell') WHERE bead = 'ovb-1';
  IF n <> 0 THEN RAISE EXCEPTION 'the cell filter leaks what the project filter refuses'; END IF;

  -- Their own work job, not the other project's.
  SELECT count(*) INTO n FROM system_queue_overview(NULL) WHERE bead = 'ova-1';
  IF n <> 1 THEN RAISE EXCEPTION 'a member cannot see their own queue job'; END IF;
  SELECT count(*) INTO n FROM system_queue_overview(NULL) WHERE bead = 'ovb-1';
  IF n <> 0 THEN RAISE EXCEPTION 'a member sees a queue job for a project they are not in'; END IF;
  SELECT count(*) INTO n FROM system_queue_overview(NULL)
   WHERE brief = 'A brief only its author should see';
  IF n <> 0 THEN RAISE EXCEPTION 'a member reads somebody else''s plan brief'; END IF;

  RAISE NOTICE 'a member sees their project and company work, and nothing else';
END $$;

DO $$
DECLARE n integer;
BEGIN
  -- The outsider to alpha: beta is theirs, alpha is not. Symmetry matters --
  -- a filter that happened to hide the second project from everybody would
  -- pass the block above.
  PERFORM set_config('workgraph.user_id', current_setting('test.outsider'), true);

  SELECT count(*) INTO n FROM system_work_overview(NULL) WHERE bead = 'ova-1';
  IF n <> 0 THEN RAISE EXCEPTION 'an outsider to alpha sees alpha''s bead'; END IF;

  -- The outsider owns beta but is not a member of it, and ownership is not
  -- readership anywhere else in this schema either.
  SELECT count(*) INTO n FROM system_work_overview(NULL) WHERE bead = 'ovb-1';
  IF n <> 0 THEN RAISE EXCEPTION 'project ownership alone granted read access'; END IF;

  -- Their own plan job, which nobody else may read.
  SELECT count(*) INTO n FROM system_queue_overview(NULL)
   WHERE brief = 'A brief only its author should see';
  IF n <> 1 THEN RAISE EXCEPTION 'the author of a plan job cannot see it'; END IF;

  RAISE NOTICE 'the filter is symmetric, and a requester sees their own job';
END $$;

DO $$
DECLARE n integer;
BEGIN
  -- Company management reads everything, as decided (mem-003b900b), including
  -- the queue rows that have no project to scope them to.
  PERFORM set_config('workgraph.user_id', current_setting('test.boss'), true);

  SELECT count(*) INTO n FROM system_work_overview(NULL)
   WHERE bead IN ('ova-1', 'ovb-1', 'ovc-1');
  IF n <> 3 THEN RAISE EXCEPTION 'company management sees % of the 3 beads', n; END IF;

  SELECT count(*) INTO n FROM system_queue_overview(NULL)
   WHERE brief = 'A brief only its author should see';
  IF n <> 1 THEN RAISE EXCEPTION 'company management cannot see a plan job'; END IF;

  RAISE NOTICE 'company management sees every project''s work and the plan queue';
END $$;

-- No identity, no rows. A definer function that answers a caller it cannot
-- identify is the hole this closes, and the API called it exactly that way:
-- it checked that a user was authenticated and then queried outside
-- authz.WithUser, so the database was asked as nobody.
DO $$
DECLARE n integer;
BEGIN
  PERFORM set_config('workgraph.user_id', '', true);
  IF current_app_user() IS NOT NULL THEN
    RAISE EXCEPTION 'this block must run with no app user';
  END IF;

  SELECT count(*) INTO n FROM system_work_overview(NULL);
  IF n <> 0 THEN RAISE EXCEPTION 'an unidentified caller reads % beads', n; END IF;
  SELECT count(*) INTO n FROM system_queue_overview(NULL);
  IF n <> 0 THEN RAISE EXCEPTION 'an unidentified caller reads % queue jobs', n; END IF;

  RAISE NOTICE 'an unidentified caller reads nothing';
END $$;

ROLLBACK;
