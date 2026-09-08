-- A dispatch runs where the work is, or does not run (wg-ugb).
--
-- Three PortalJS beads were dispatched on 4 September and ran against
-- datopian/workgraph-agent-sandbox. The handler defaulted the cell to "oss" and
-- passed the rig through unset; the dispatcher fell back to its own default rig.
-- The agent searched for PortalJS source, correctly found none, and said so --
-- after 48 model calls and about 78 cents.
--
-- project_repositories already knew which repositories a project has. What was
-- missing was the other half of the join: nothing recorded which repository a
-- RIG holds. Every case below is one of the answers that join has to give.

BEGIN;

DO $$
DECLARE
  org uuid; node uuid; cell uuid; u uuid; backup uuid;
  proj uuid; empty_proj uuid; n integer; got text;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  INSERT INTO execution_nodes (hostname, environment)
  VALUES ('routing-probe.invalid', 'staging')
  ON CONFLICT (hostname) DO UPDATE SET environment = EXCLUDED.environment
  RETURNING id INTO node;
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  VALUES (node, 'route-cell', 'wgcell_route', 'oss')
  RETURNING id INTO cell;

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Route Owner', 'route-owner@example.invalid') RETURNING id INTO u;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Route Backup', 'route-backup@example.invalid') RETURNING id INTO backup;

  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id,
                        execution_cell_id)
  VALUES (org, 'route-proj', 'Routed Project', u, backup, cell) RETURNING id INTO proj;
  INSERT INTO project_repositories (project_id, provider, owner, name)
  VALUES (proj, 'github', 'datopian', 'route-repo');

  -- A second project on the same cell WITH no repository, which is the other
  -- refusal and has a different fix: attach a repository rather than
  -- provision a rig. cdt is in exactly this state today.
  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id,
                        execution_cell_id)
  VALUES (org, 'route-empty', 'Project With No Repo', u, backup, cell)
  RETURNING id INTO empty_proj;

  PERFORM system_project_bead('route-cell', 'rt-1', 'Routable', 'task', 'open',
                              ARRAY['wg-project-route-proj']);
  PERFORM system_project_bead('route-cell', 'rt-none', 'No repo', 'task', 'open',
                              ARRAY['wg-project-route-empty']);
  PERFORM system_project_bead('route-cell', 'rt-company', 'Company work', 'task', 'open',
                              ARRAY['scope:company']);

  -- Nothing is registered yet, so nothing routes. This is the state every cell
  -- starts in, and it must refuse rather than guess.
  SELECT count(*) INTO n FROM system_rigs_for_bead('rt-1', 'route-cell');
  IF n <> 0 THEN
    RAISE EXCEPTION 'a bead routed to % rig(s) before any rig was registered', n;
  END IF;

  -- A rig holding an UNRELATED repository must not match. This is the bug: the
  -- sandbox rig holds the agent sandbox, and a PortalJS bead ran there anyway.
  PERFORM system_register_rig('route-cell', 'sandbox', 'github', 'datopian', 'unrelated-sandbox');
  SELECT count(*) INTO n FROM system_rigs_for_bead('rt-1', 'route-cell');
  IF n <> 0 THEN
    RAISE EXCEPTION 'a bead routed to a rig holding somebody else''s repository';
  END IF;

  -- A rig holding the project's repository matches.
  PERFORM system_register_rig('route-cell', 'routed', 'github', 'datopian', 'route-repo');
  SELECT rig INTO got FROM system_rigs_for_bead('rt-1', 'route-cell');
  IF got <> 'routed' THEN
    RAISE EXCEPTION 'the bead routed to %, want routed', got;
  END IF;

  -- A rig with NO repository never matches. The witness and the mayor are rigs
  -- with no working tree, and routing work into one would be worse than
  -- refusing.
  PERFORM system_register_rig('route-cell', 'witness');
  SELECT count(*) INTO n FROM system_rigs_for_bead('rt-1', 'route-cell');
  IF n <> 1 THEN
    RAISE EXCEPTION 'a rig with no repository joined the candidates (% total)', n;
  END IF;

  -- Two rigs holding the same project's repositories is ambiguous, and the
  -- handler asks rather than picking: choosing arbitrarily hides the ambiguity
  -- until somebody wonders why their work ran in the wrong checkout.
  INSERT INTO project_repositories (project_id, provider, owner, name)
  VALUES (proj, 'github', 'datopian', 'route-repo-two');
  PERFORM system_register_rig('route-cell', 'routed-two', 'github', 'datopian', 'route-repo-two');
  SELECT count(*) INTO n FROM system_rigs_for_bead('rt-1', 'route-cell');
  IF n <> 2 THEN
    RAISE EXCEPTION 'two rigs hold this project''s repositories and % matched', n;
  END IF;

  -- BOTH must be named. The refusal a caller reads is built from this list,
  -- and now that naming a rig is how the ambiguity is resolved, a list that
  -- omits one offers a choice that cannot be made. This is the ordinary case,
  -- not a corner: the PortalJS project holds datopian/portaljs AND
  -- datopian/cloud.portaljs.com.
  SELECT string_agg(rig, ',' ORDER BY rig) INTO got
    FROM system_rigs_for_bead('rt-1', 'route-cell');
  IF got <> 'routed,routed-two' THEN
    RAISE EXCEPTION 'the candidate rigs were % rather than both', got;
  END IF;

  -- A project with no repository at all routes nowhere, and the handler says so
  -- differently: the fix is to attach a repository, not to provision a rig.
  SELECT count(*) INTO n FROM system_rigs_for_bead('rt-none', 'route-cell');
  IF n <> 0 THEN
    RAISE EXCEPTION 'a project with no repository routed to % rig(s)', n;
  END IF;

  -- A bead with NO project is not routed and not refused. Company-wide work has
  -- no repository to match, and refusing it would break the case that worked
  -- before any of this existed.
  SELECT count(*) INTO n FROM system_rigs_for_bead('rt-company', 'route-cell');
  IF n <> 0 THEN
    RAISE EXCEPTION 'a company-scoped bead matched % rig(s); it should not be routed at all', n;
  END IF;

  -- Re-registering a rig REPLACES what it holds rather than accumulating. A rig
  -- re-pointed at another repository must stop claiming the old one.
  PERFORM system_register_rig('route-cell', 'routed', 'github', 'datopian', 'unrelated-sandbox');
  SELECT count(*) INTO n FROM system_rigs_for_bead('rt-1', 'route-cell');
  IF n <> 1 THEN
    RAISE EXCEPTION 'a re-pointed rig still claims its old repository (% matched)', n;
  END IF;

  -- Half a repository is refused by the schema: an owner with no name cannot be
  -- joined and would silently match nothing, which looks exactly like a rig
  -- that holds nothing.
  BEGIN
    INSERT INTO execution_rigs (execution_cell_id, rig, provider, owner)
    VALUES (cell, 'half', 'github', 'datopian');
    RAISE EXCEPTION 'a rig with an owner and no repository name was accepted';
  EXCEPTION WHEN check_violation THEN
    NULL;
  END;

  -- A bead id that exists NOWHERE is refused rather than dispatched.
  --
  -- Two situations used to give the same answer: a bead with no project
  -- (ordinary, company-wide work, routed to the cell's default rig) and an id
  -- that matches nothing at all (a typo). The second was accepted, claimed,
  -- given to an agent and billed -- observed when an unquoted shell variable
  -- turned `dispatch $BEAD oss` into `dispatch oss`, and a bead named `oss`
  -- ran against nothing.
  --
  -- Asserted through work_refs rather than through the handler, because the
  -- handler's own decision is tested in Go; what matters here is that the two
  -- states are distinguishable in the data at all.
  SELECT count(*) INTO n FROM work_refs WHERE bead_id = 'rt-nonexistent';
  IF n <> 0 THEN
    RAISE EXCEPTION 'the fixture is wrong: rt-nonexistent should not be projected';
  END IF;
  -- rt-company is the case the guard must PRESERVE: projected, belonging to no
  -- project, and legitimately routed to the cell's default rig. If the guard
  -- caught this it would break company-wide work, which is the whole reason
  -- the two states have to be told apart rather than both refused.
  SELECT count(*) INTO n FROM work_refs WHERE bead_id = 'rt-company';
  IF n <> 1 THEN
    RAISE EXCEPTION 'rt-company should be projected, and is % rows', n;
  END IF;
  IF (SELECT project_id FROM work_refs WHERE bead_id = 'rt-company') IS NOT NULL THEN
    RAISE EXCEPTION 'rt-company has a project, so it is not the company-wide case';
  END IF;
  -- The distinguishing query the router runs. Projected-with-no-project is a
  -- different fact from never-projected, and both must be answerable.
  IF NOT EXISTS (SELECT 1 FROM work_refs WHERE bead_id = 'rt-company') THEN
    RAISE EXCEPTION 'a bead with no project is not visible as projected';
  END IF;
  IF EXISTS (SELECT 1 FROM work_refs WHERE bead_id = 'rt-nonexistent') THEN
    RAISE EXCEPTION 'an id that was never projected appears projected';
  END IF;

  RAISE NOTICE 'dispatch routing: unrelated rig refused, matching rig found, two candidates both offered, empty and company beads not routed';
END $$;

ROLLBACK;
