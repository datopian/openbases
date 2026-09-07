-- Where an agent's work went, and who may see it.
--
-- The question this answers was asked of sa-kfh: it renamed a hero tab label,
-- closed its bead, and the change was nowhere in the GitHub repository. The
-- honest answer at the time was that nothing an agent wrote could reach a
-- repository at all, and nothing in the interface said so.
--
-- Every case below is a way the record of a landing could be wrong: attributed
-- to a repository the rig does not hold, duplicated by a re-dispatch, claiming
-- to have landed on the base branch, or readable by somebody who cannot see
-- the project the work belongs to.

BEGIN;

DO $$
DECLARE
  org uuid; node uuid; cell uuid; other uuid; u uuid; backup uuid; outsider uuid;
  proj uuid; n integer; got text; pr bead_pull_requests;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  INSERT INTO execution_nodes (hostname, environment)
  VALUES ('landing-probe.invalid', 'staging')
  ON CONFLICT (hostname) DO UPDATE SET environment = EXCLUDED.environment
  RETURNING id INTO node;
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  VALUES (node, 'land-cell', 'wgcell_land', 'oss') RETURNING id INTO cell;
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  VALUES (node, 'land-other', 'wgcell_land_other', 'oss') RETURNING id INTO other;

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Land Owner', 'land-owner@example.invalid') RETURNING id INTO u;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Land Backup', 'land-backup@example.invalid') RETURNING id INTO backup;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Land Outsider', 'land-outsider@example.invalid') RETURNING id INTO outsider;

  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id,
                        execution_cell_id)
  VALUES (org, 'land-proj', 'Landing Project', u, backup, cell) RETURNING id INTO proj;
  INSERT INTO project_memberships (project_id, user_id, role_name)
  VALUES (proj, u, 'project_lead');
  INSERT INTO project_repositories (project_id, provider, owner, name)
  VALUES (proj, 'github', 'datopian', 'land-repo');

  PERFORM system_register_rig('land-cell', 'landrig', 'github', 'datopian', 'land-repo');

  -- Projected the way a real bead arrives, rather than inserted: work_refs
  -- carries a beads_database_id and an attribution the projection function
  -- works out from the label, and a test that side-steps it is testing a
  -- shape the system never produces.
  PERFORM system_project_bead('land-cell', 'ld-1', 'Rename the hero tab', 'task', 'closed',
          ARRAY['wg-project-land-proj']);

  -- The repository is taken from the RIG, not from the caller. A node that
  -- could name its own repository could record a pull request against
  -- somebody else's code.
  pr := system_record_pull_request('land-cell', 'landrig', 'ld-1', 7,
        'https://github.com/datopian/land-repo/pull/7', 'bead/ld-1', 'main');
  IF pr.owner <> 'datopian' OR pr.name <> 'land-repo' THEN
    RAISE EXCEPTION 'the pull request was attributed to %/%', pr.owner, pr.name;
  END IF;

  -- A rig holding no repository cannot have landed anything, and saying so is
  -- better than recording a row that names nothing.
  PERFORM system_register_rig('land-cell', 'bare', NULL, NULL, NULL);
  BEGIN
    PERFORM system_record_pull_request('land-cell', 'bare', 'ld-1', 8,
            'https://example.invalid/8', 'bead/ld-1-b', 'main');
    RAISE EXCEPTION 'a rig holding no repository recorded a pull request';
  EXCEPTION WHEN raise_exception THEN
    IF position('holds no repository' IN SQLERRM) = 0 THEN RAISE; END IF;
  END;

  -- A rig on ANOTHER cell is not this cell's rig, even by the same name.
  BEGIN
    PERFORM system_record_pull_request('land-other', 'landrig', 'ld-1', 9,
            'https://example.invalid/9', 'bead/ld-1-c', 'main');
    RAISE EXCEPTION 'a rig on another cell recorded a pull request here';
  EXCEPTION WHEN raise_exception THEN
    IF position('holds no repository' IN SQLERRM) = 0 THEN RAISE; END IF;
  END;

  -- Re-landing the same branch does NOT make a second pull request. This is
  -- the ordinary consequence of re-dispatching a bead: another commit on the
  -- same branch. Multiplying pull requests is how a review queue becomes noise
  -- nobody reads.
  pr := system_record_pull_request('land-cell', 'landrig', 'ld-1', 7,
        'https://github.com/datopian/land-repo/pull/7', 'bead/ld-1', 'main');
  SELECT count(*) INTO n FROM bead_pull_requests WHERE bead = 'ld-1';
  IF n <> 1 THEN
    RAISE EXCEPTION 'landing the same branch twice recorded % rows', n;
  END IF;

  -- A DIFFERENT branch is a different pull request, because it is.
  PERFORM system_record_pull_request('land-cell', 'landrig', 'ld-1', 11,
          'https://github.com/datopian/land-repo/pull/11', 'bead/ld-1-retry', 'main');
  SELECT count(*) INTO n FROM bead_pull_requests WHERE bead = 'ld-1';
  IF n <> 2 THEN
    RAISE EXCEPTION 'a second branch recorded % rows for the bead', n;
  END IF;

  -- Nothing may claim to have landed on the base branch. This table is the
  -- audit trail of where agent work went, and a row saying the work went
  -- straight onto main would be a lie in exactly the record somebody would
  -- consult to find out whether it had.
  BEGIN
    INSERT INTO bead_pull_requests
      (bead, execution_cell_id, rig, owner, name, number, url, head, base)
    VALUES ('ld-1', cell, 'landrig', 'datopian', 'land-repo', 12,
            'https://example.invalid/12', 'main', 'main');
    RAISE EXCEPTION 'a pull request from main onto main was recorded';
  EXCEPTION WHEN check_violation THEN
    NULL;
  END;

  -- The bead's detail carries them, newest first, so "did it land" is
  -- answerable from the interface rather than by reading a node's working tree.
  PERFORM set_config('workgraph.user_id', u::text, true);
  SELECT jsonb_array_length(system_bead_detail('ld-1') -> 'pull_requests') INTO n;
  IF n <> 2 THEN
    RAISE EXCEPTION 'the bead detail shows % pull requests, want 2', n;
  END IF;
  SELECT system_bead_detail('ld-1') -> 'pull_requests' -> 0 ->> 'head' INTO got;
  IF got <> 'bead/ld-1-retry' THEN
    RAISE EXCEPTION 'the newest pull request is not first (got %)', got;
  END IF;
  SELECT system_bead_detail('ld-1') -> 'pull_requests' -> 0 ->> 'repository' INTO got;
  IF got <> 'datopian/land-repo' THEN
    RAISE EXCEPTION 'the detail names the repository as %', got;
  END IF;

  -- A bead that ran, landed a change, and is still open reads as `landed`,
  -- not `blocked`. Those need different words: one asks somebody to unblock
  -- an agent, the other asks them to review a diff. sa-kfh was reported as
  -- blocked while its change was open on datopian/portaljs#1662.
  INSERT INTO work_queue (kind, cell, rig, bead, status, claimed_at, finished_at)
  VALUES ('work', 'land-cell', 'landrig', 'ld-1', 'done', now(), now());
  UPDATE work_refs SET status = 'open' WHERE bead_id = 'ld-1';
  PERFORM set_config('workgraph.user_id', u::text, true);
  SELECT system_bead_detail('ld-1') ->> 'outcome' INTO got;
  IF got <> 'landed' THEN
    RAISE EXCEPTION 'a bead whose work is in a pull request reads as %', got;
  END IF;

  -- And a CLOSED bead with a pull request is `done`, which is the better
  -- answer. The landed case must not shadow it.
  UPDATE work_refs SET status = 'closed' WHERE bead_id = 'ld-1';
  SELECT system_bead_detail('ld-1') ->> 'outcome' INTO got;
  IF got <> 'done' THEN
    RAISE EXCEPTION 'a closed bead with a pull request reads as %', got;
  END IF;

  -- An open bead with NO pull request is still blocked, because it is. Its
  -- own bead rather than ld-1 with its rows deleted: the RLS section below
  -- counts ld-1's pull requests, and a test that quietly removes the state a
  -- later assertion depends on fails somewhere other than where it is wrong.
  PERFORM system_project_bead('land-cell', 'ld-3', 'Could not proceed', 'task', 'open',
          ARRAY['wg-project-land-proj']);
  INSERT INTO work_queue (kind, cell, rig, bead, status, claimed_at, finished_at)
  VALUES ('work', 'land-cell', 'landrig', 'ld-3', 'done', now(), now());
  SELECT system_bead_detail('ld-3') ->> 'outcome' INTO got;
  IF got <> 'blocked' THEN
    RAISE EXCEPTION 'an open bead with no pull request reads as %', got;
  END IF;

  -- ld-1 is left CLOSED, which is how the case above leaves it and what the
  -- RLS section below expects to still find two pull requests for.

  -- A bead that landed nothing says so with an empty list rather than a null,
  -- so a reader does not have to distinguish "no pull requests" from "this
  -- field is missing".
  PERFORM system_project_bead('land-cell', 'ld-2', 'Needed no code change', 'task', 'closed',
          ARRAY['wg-project-land-proj']);
  IF system_bead_detail('ld-2') -> 'pull_requests' <> '[]'::jsonb THEN
    RAISE EXCEPTION 'a bead that landed nothing does not report an empty list';
  END IF;

  RAISE NOTICE 'landing: attributed from the rig, idempotent per branch, never onto the base';
END $$;

-- And the RLS boundary, evaluated as the application role rather than as the
-- superuser CI runs as. A pull request URL names a repository and a branch, so
-- it says something about work the reader may not be entitled to see.

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE member uuid; outsider uuid; n integer;
BEGIN
  SELECT id INTO member FROM users WHERE primary_email = 'land-owner@example.invalid';
  SELECT id INTO outsider FROM users WHERE primary_email = 'land-outsider@example.invalid';

  PERFORM set_config('workgraph.user_id', member::text, true);
  SELECT count(*) INTO n FROM bead_pull_requests WHERE bead = 'ld-1';
  IF n <> 2 THEN
    RAISE EXCEPTION 'a member of the project sees % of its pull requests, want 2', n;
  END IF;

  PERFORM set_config('workgraph.user_id', outsider::text, true);
  SELECT count(*) INTO n FROM bead_pull_requests WHERE bead = 'ld-1';
  IF n <> 0 THEN
    RAISE EXCEPTION 'somebody outside the project sees % of its pull requests', n;
  END IF;

  PERFORM set_config('workgraph.user_id', '', true);
  SELECT count(*) INTO n FROM bead_pull_requests;
  IF n <> 0 THEN
    RAISE EXCEPTION 'an unauthenticated caller sees % pull requests', n;
  END IF;

  RAISE NOTICE 'landing visibility: member yes, outsider no, nobody no';
END $$;

-- And the lookup the pull request endpoint makes, as the caller that actually
-- makes it: a NODE, which is a service credential with no app user at all.
--
-- This is the case nothing covered, and it cost the first landing its pull
-- request. execution_rigs has RLS forced and its policy is
-- `current_app_user() IS NOT NULL`, so a direct read by a node matches nothing
-- and a rig recorded as holding datopian/workgraph-agent-sandbox reports as
-- holding no repository. The earlier sections all passed because they call
-- SECURITY DEFINER functions, which is exactly what the endpoint did not do.

DO $$
DECLARE v_owner text; v_name text; n integer;
BEGIN
  -- No app user: this is a node, not a person.
  PERFORM set_config('workgraph.user_id', '', true);

  -- The direct read the endpoint used to do. Asserted to see NOTHING, so that
  -- if the policy is ever loosened this test says so rather than quietly
  -- agreeing with a weaker rule.
  SELECT count(*) INTO n FROM execution_rigs;
  IF n <> 0 THEN
    RAISE EXCEPTION 'a service caller reads execution_rigs directly (% rows); '
      'the endpoint''s inline query would appear to work and the policy has changed', n;
  END IF;

  -- And the function, which is how it must be asked.
  SELECT owner, name INTO v_owner, v_name FROM system_rig_repository('land-cell', 'landrig');
  IF v_owner IS DISTINCT FROM 'datopian' OR v_name IS DISTINCT FROM 'land-repo' THEN
    RAISE EXCEPTION 'a node cannot see the repository its rig holds (got %/%)', v_owner, v_name;
  END IF;

  -- A rig holding half a repository holds none, because an owner with no name
  -- matches no project and looks identical to a rig that holds nothing.
  SELECT count(*) INTO n FROM system_rig_repository('land-cell', 'bare');
  IF n <> 0 THEN
    RAISE EXCEPTION 'a rig holding no repository reported one';
  END IF;

  -- And a rig on another cell is not this cell's, even by the same name.
  SELECT count(*) INTO n FROM system_rig_repository('land-other', 'landrig');
  IF n <> 0 THEN
    RAISE EXCEPTION 'a rig on another cell was reported as this cell''s';
  END IF;

  RAISE NOTICE 'a node can ask which repository its rig holds, through the function and not otherwise';
END $$;

RESET ROLE;

ROLLBACK;
