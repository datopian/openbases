-- The router can see a bead that belongs to a project.
--
-- This is the test that was missing, and its absence let dispatch tell people
-- the opposite of the truth for every project-scoped bead.
--
-- internal/dispatchroute explains an unroutable bead by reading work_refs. That
-- read used to be direct, and work_refs has RLS forced: with no app user a row
-- is visible only when project_id IS NULL. So a bead with a project looked
-- ABSENT, and dispatch answered bead_not_projected -- "either the id is wrong,
-- or it was created seconds ago... wait a few seconds and try again" -- for a
-- bead that had been projected for hours and whose project had a repository.
-- Eight beads in project msf were refused that way.
--
-- Every earlier dispatch happened to use a projectless bead, which is the one
-- case the policy lets through, so nothing failed until somebody filed work
-- under a project.
--
-- Dropping to workgraph_app is the whole point: as the owner, RLS is bypassed
-- and this test cannot fail.
BEGIN;

DO $$
DECLARE
  org uuid; node uuid; cell uuid; u uuid; backup uuid; proj uuid; graph uuid;
  v_projected boolean; v_project text; v_repos integer;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  INSERT INTO execution_nodes (hostname, environment)
  VALUES ('rls-routing-probe.invalid', 'staging')
  ON CONFLICT (hostname) DO UPDATE SET environment = EXCLUDED.environment
  RETURNING id INTO node;
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  VALUES (node, 'rls-route-cell', 'wgcell_rlsroute', 'oss')
  RETURNING id INTO cell;

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'RLS Route Owner', 'rls-route-owner@example.invalid') RETURNING id INTO u;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'RLS Route Backup', 'rls-route-backup@example.invalid') RETURNING id INTO backup;

  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id,
                        execution_cell_id)
  VALUES (org, 'rls-route-proj', 'RLS Routed Project', u, backup, cell) RETURNING id INTO proj;
  INSERT INTO project_repositories (project_id, provider, owner, name)
  VALUES (proj, 'github', 'datopian', 'rls-route-repo');

  INSERT INTO beads_databases (organisation_id, execution_cell_id, name, scope, project_id)
  VALUES (org, cell, 'rls-route-graph', 'project', proj) RETURNING id INTO graph;

  -- The bead under test: projected, and belonging to a project.
  INSERT INTO work_refs (organisation_id, beads_database_id, bead_id, title, kind,
                         status, project_id)
  VALUES (org, graph, 'rr-001', 'a bead that belongs to a project', 'task', 'open', proj);

  -- And one with no project, which is the case that always worked and must
  -- keep working.
  INSERT INTO work_refs (organisation_id, beads_database_id, bead_id, title, kind,
                         status, project_id)
  VALUES (org, graph, 'rr-002', 'a bead that belongs to nothing', 'task', 'open', NULL);

  -- From here on, as the role the dispatch path actually connects as, with no
  -- app user -- exactly the conditions under which this was wrong.
  SET LOCAL ROLE workgraph_app;
  IF current_user <> 'workgraph_app' THEN
      RAISE EXCEPTION 'expected to be workgraph_app, am %', current_user;
  END IF;
  IF current_app_user() IS NOT NULL THEN
      RAISE EXCEPTION 'expected no app user, got %', current_app_user();
  END IF;

  SELECT projected, project, repositories INTO v_projected, v_project, v_repos
    FROM system_bead_routing('rr-001');
  IF NOT v_projected THEN
      RAISE EXCEPTION 'a project-scoped bead reads as NOT projected; dispatch would '
          'answer bead_not_projected and tell somebody to wait for a pass that '
          'has already run';
  END IF;
  IF v_project <> 'rls-route-proj' THEN
      RAISE EXCEPTION 'the bead''s project reads as %, not rls-route-proj', quote_nullable(v_project);
  END IF;
  IF v_repos <> 1 THEN
      RAISE EXCEPTION 'the project''s repository count reads as %, not 1; dispatch would '
          'tell somebody to attach a repository that is already attached', v_repos;
  END IF;

  -- The projectless bead: projected, and no project to route on. Dispatch
  -- honours a named rig for this one rather than refusing.
  SELECT projected, project INTO v_projected, v_project
    FROM system_bead_routing('rr-002');
  IF NOT v_projected OR v_project <> '' THEN
      RAISE EXCEPTION 'a projectless bead reads as projected=%, project=%',
          v_projected, quote_nullable(v_project);
  END IF;

  -- An id that exists nowhere must still read as not projected, or the guard
  -- against dispatching a typo is gone.
  SELECT projected INTO v_projected FROM system_bead_routing('rr-nope');
  IF v_projected THEN
      RAISE EXCEPTION 'an id that exists nowhere reads as projected; a typo would be '
          'dispatched and billed';
  END IF;
END
$$;

ROLLBACK;

SELECT 'the router sees a project-scoped bead, a projectless one, and neither for a typo' AS result;
