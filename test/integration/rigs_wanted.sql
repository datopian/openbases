-- A cell should have one rig per repository its projects hold (wg-ugb).
--
-- 0084 made dispatch refuse when no rig held the work. This is the list that
-- makes the refusal fixable: the rigs a cell should have, which the playbook
-- provisions. Every case below is a way the derivation could quietly go wrong
-- and produce a rig nobody can route to, or two rigs holding one repository.

BEGIN;

DO $$
DECLARE
  org uuid; node uuid; cell uuid; other_cell uuid; u uuid; backup uuid;
  proj uuid; second uuid; archived uuid; n integer; got text;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  INSERT INTO execution_nodes (hostname, environment)
  VALUES ('wanted-probe.invalid', 'staging')
  ON CONFLICT (hostname) DO UPDATE SET environment = EXCLUDED.environment
  RETURNING id INTO node;
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  VALUES (node, 'want-cell', 'wgcell_want', 'oss') RETURNING id INTO cell;
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  VALUES (node, 'want-other', 'wgcell_want_other', 'oss') RETURNING id INTO other_cell;

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Want Owner', 'want-owner@example.invalid') RETURNING id INTO u;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Want Backup', 'want-backup@example.invalid') RETURNING id INTO backup;

  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id,
                        execution_cell_id)
  VALUES (org, 'want-proj', 'Wanted Project', u, backup, cell) RETURNING id INTO proj;
  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id,
                        execution_cell_id)
  VALUES (org, 'want-second', 'Second Wanted Project', u, backup, cell) RETURNING id INTO second;

  -- A name that needs folding, because a rig name reaches a directory, a
  -- systemd unit and a branch prefix. autoclaw.sh must not become a rig called
  -- `autoclaw.sh`, and it must not be truncated at the dot either.
  INSERT INTO project_repositories (project_id, provider, owner, name)
  VALUES (proj, 'github', 'datopian', 'Want.Portal-Site');
  SELECT rig INTO got FROM system_rigs_wanted('want-cell');
  IF got <> 'want_portal_site' THEN
    RAISE EXCEPTION 'a repository name was not folded into a safe rig name (got %)', got;
  END IF;

  -- The clone URL is built, not stored, and a rig cloned from the wrong URL is
  -- a rig holding the wrong code.
  SELECT clone_url INTO got FROM system_rigs_wanted('want-cell');
  IF got <> 'https://github.com/datopian/Want.Portal-Site.git' THEN
    RAISE EXCEPTION 'the clone URL was built wrongly (got %)', got;
  END IF;

  -- Another cell's repositories are not this cell's business. A cell that
  -- provisioned every project's repositories would clone the client work onto
  -- the open-source node, which is the isolation the cells exist for.
  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id,
                        execution_cell_id)
  VALUES (org, 'want-elsewhere', 'Elsewhere', u, backup, other_cell) RETURNING id INTO archived;
  INSERT INTO project_repositories (project_id, provider, owner, name)
  VALUES (archived, 'github', 'datopian', 'elsewhere-repo');
  SELECT count(*) INTO n FROM system_rigs_wanted('want-cell');
  IF n <> 1 THEN
    RAISE EXCEPTION 'a cell wants another cell''s repositories (% rows)', n;
  END IF;

  -- One repository cannot want two rigs, and the schema is why: a repository
  -- is unique on (provider, owner, name) across the whole registry, so it
  -- belongs to exactly one project. Asserted rather than assumed, because the
  -- derivation below would otherwise need to deduplicate and the day that
  -- constraint is relaxed is the day two rigs start holding one repository.
  BEGIN
    INSERT INTO project_repositories (project_id, provider, owner, name)
    VALUES (second, 'github', 'datopian', 'Want.Portal-Site');
    RAISE EXCEPTION 'one repository was accepted into two projects';
  EXCEPTION WHEN unique_violation THEN
    NULL;
  END;

  -- Same repository name under different owners: qualified by owner, because
  -- the bare name would put two different checkouts in one directory and one
  -- of them would silently be the wrong code.
  INSERT INTO project_repositories (project_id, provider, owner, name)
  VALUES (second, 'github', 'someone-else', 'Want.Portal-Site');
  SELECT count(*) INTO n FROM system_rigs_wanted('want-cell');
  IF n <> 2 THEN
    RAISE EXCEPTION 'a name collision under two owners produced % rigs', n;
  END IF;
  SELECT count(DISTINCT rig) INTO n FROM system_rigs_wanted('want-cell');
  IF n <> 2 THEN
    RAISE EXCEPTION 'two owners of one repository name share a rig name';
  END IF;

  -- Prefixes are unique, because a bead id is only unambiguous through its
  -- prefix and two rigs sharing one puts two graphs in one id space. Three
  -- characters of the name alone collided on the real oss cell the first time
  -- this ran: datahub-next and data-portal-examples both give `dat`.
  SELECT count(*) INTO n FROM (
    SELECT prefix FROM system_rigs_wanted('want-cell') GROUP BY prefix HAVING count(*) > 1
  ) dupes;
  IF n <> 0 THEN
    RAISE EXCEPTION '% prefix collision(s) among the wanted rigs', n;
  END IF;

  -- A rig that already holds the repository keeps ITS name, whatever the
  -- derivation would have called it. The oss cell's `autoclaw` holds
  -- autoclaw.sh, which derives `autoclaw_sh`: without this the playbook would
  -- clone the same repository into a second rig on the next run.
  PERFORM system_register_rig('want-cell', 'legacy-name', 'github', 'datopian', 'Want.Portal-Site');
  SELECT rig INTO got FROM system_rigs_wanted('want-cell')
   WHERE owner = 'datopian' AND name = 'Want.Portal-Site';
  IF got <> 'legacy-name' THEN
    RAISE EXCEPTION 'a repository already held by a rig was renamed to % ', got;
  END IF;
  SELECT count(*) INTO n FROM system_rigs_wanted('want-cell') WHERE held;
  IF n <> 1 THEN
    RAISE EXCEPTION 'a repository held by a rig was not reported as held (% held)', n;
  END IF;

  -- Matching is case-insensitive on owner and name, because the node reports
  -- what git says and the registry holds what somebody typed. GitHub treats
  -- them as one repository, and a case difference must not provision a second
  -- rig for it.
  PERFORM system_register_rig('want-cell', 'cased', 'github', 'SOMEONE-ELSE', 'want.portal-site');
  SELECT count(*) INTO n FROM system_rigs_wanted('want-cell') WHERE held;
  IF n <> 2 THEN
    RAISE EXCEPTION 'a repository differing only in case was not matched (% held)', n;
  END IF;

  -- A rig registered on ANOTHER cell does not satisfy this cell. Rigs are
  -- directories on one node; a rig on the client node cannot run oss work.
  PERFORM system_register_rig('want-other', 'far', 'github', 'datopian', 'want-far');
  INSERT INTO project_repositories (project_id, provider, owner, name)
  VALUES (proj, 'github', 'datopian', 'want-far');
  SELECT count(*) INTO n FROM system_rigs_wanted('want-cell')
   WHERE name = 'want-far' AND held;
  IF n <> 0 THEN
    RAISE EXCEPTION 'a rig on another cell was counted as held here';
  END IF;

  -- A CLOSED project's repositories are not wanted: its work is over and
  -- cloning it is disk spent on nothing.
  UPDATE projects SET status = 'closed' WHERE id = second;
  SELECT count(*) INTO n FROM system_rigs_wanted('want-cell')
   WHERE owner = 'someone-else';
  IF n <> 0 THEN
    RAISE EXCEPTION 'a closed project still wants a rig';
  END IF;

  -- A PAUSED one's are. system_rigs_for_bead does not look at status, so a
  -- paused project can still be dispatched to; wanting no rig for it would
  -- leave it routable and unprovisioned, which is the refusal this exists to
  -- remove.
  UPDATE projects SET status = 'paused' WHERE id = second;
  SELECT count(*) INTO n FROM system_rigs_wanted('want-cell')
   WHERE owner = 'someone-else';
  IF n <> 1 THEN
    RAISE EXCEPTION 'a paused project wants % rigs, so it could be dispatched to and not provisioned', n;
  END IF;

  -- A cell that does not exist wants nothing, rather than wanting everything.
  -- The join is on the cell, and a typo in a playbook variable must not clone
  -- every repository in the registry onto one node.
  SELECT count(*) INTO n FROM system_rigs_wanted('no-such-cell');
  IF n <> 0 THEN
    RAISE EXCEPTION 'an unknown cell wanted % rigs', n;
  END IF;

  RAISE NOTICE 'rigs wanted: names folded, owners qualified, prefixes unique, held rigs keep their names';
END $$;

ROLLBACK;
