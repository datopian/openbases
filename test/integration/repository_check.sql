-- The command that says whether a change works (opt-in, per repository).
--
-- An agent has no shell, so it cannot build or test what it writes. This is
-- the command the node runs afterwards. Two properties matter here and they
-- pull in opposite directions: a node must be able to read it, and enabling it
-- must be a deliberate act rather than a default -- because the command runs
-- code from the repository, as the cell user, after an agent could have edited
-- that code.

BEGIN;

DO $$
DECLARE
  org uuid; node uuid; cell uuid; u uuid; backup uuid; proj uuid; got text; n integer;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  INSERT INTO execution_nodes (hostname, environment)
  VALUES ('check-probe.invalid', 'staging')
  ON CONFLICT (hostname) DO UPDATE SET environment = EXCLUDED.environment
  RETURNING id INTO node;
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  VALUES (node, 'check-cell', 'wgcell_check', 'oss') RETURNING id INTO cell;

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Check Owner', 'check-owner@example.invalid') RETURNING id INTO u;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Check Backup', 'check-backup@example.invalid') RETURNING id INTO backup;

  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id,
                        execution_cell_id)
  VALUES (org, 'check-proj', 'Checked Project', u, backup, cell) RETURNING id INTO proj;
  INSERT INTO project_repositories (project_id, provider, owner, name)
  VALUES (proj, 'github', 'datopian', 'check-repo');

  PERFORM system_register_rig('check-cell', 'checkrig', 'github', 'datopian', 'check-repo');

  -- Nothing configured is the state every repository starts in, and it must
  -- read as NULL rather than as an empty command: a caller that tested for
  -- "configured" on an empty string would run nothing and call it a check.
  SELECT system_repository_check('check-cell', 'checkrig') INTO got;
  IF got IS NOT NULL THEN
    RAISE EXCEPTION 'an unconfigured repository reported the command %', got;
  END IF;

  -- An empty command is refused by the schema for the same reason.
  BEGIN
    UPDATE project_repositories SET check_command = '   '
     WHERE project_id = proj AND name = 'check-repo';
    RAISE EXCEPTION 'a whitespace check command was accepted';
  EXCEPTION WHEN check_violation THEN
    NULL;
  END;

  UPDATE project_repositories SET check_command = 'npm ci --ignore-scripts && npm test'
   WHERE project_id = proj AND name = 'check-repo';

  SELECT system_repository_check('check-cell', 'checkrig') INTO got;
  IF got <> 'npm ci --ignore-scripts && npm test' THEN
    RAISE EXCEPTION 'the command came back as %', got;
  END IF;

  -- Asked by RIG, and matched case-insensitively on the repository, because
  -- the node reports what git says and the registry holds what somebody typed.
  PERFORM system_register_rig('check-cell', 'casedrig', 'github', 'DATOPIAN', 'Check-Repo');
  SELECT system_repository_check('check-cell', 'casedrig') INTO got;
  IF got IS NULL THEN
    RAISE EXCEPTION 'a repository differing only in case reported no command';
  END IF;

  -- A rig holding no repository has nothing to check.
  PERFORM system_register_rig('check-cell', 'barerig', NULL, NULL, NULL);
  SELECT system_repository_check('check-cell', 'barerig') INTO got;
  IF got IS NOT NULL THEN
    RAISE EXCEPTION 'a rig holding no repository reported the command %', got;
  END IF;

  -- An unknown cell or rig reports nothing rather than somebody else's
  -- command. A command is a command to EXECUTE, so answering the wrong one is
  -- worse than answering none.
  SELECT count(*) INTO n FROM (
    SELECT system_repository_check('no-such-cell', 'checkrig') AS c
    UNION ALL SELECT system_repository_check('check-cell', 'no-such-rig')
  ) q WHERE c IS NOT NULL;
  IF n <> 0 THEN
    RAISE EXCEPTION 'an unknown cell or rig reported a command';
  END IF;

  RAISE NOTICE 'repository check: opt-in, per repository, asked by rig, never guessed';
END $$;

-- And a node can read it. A node is a service credential with no app user, so
-- a direct read of the RLS-protected table hands it nothing -- which is how
-- the pull request endpoint reported a rig as holding no repository (0087).

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE got text; n integer;
BEGIN
  PERFORM set_config('workgraph.user_id', '', true);

  SELECT count(*) INTO n FROM project_repositories;
  IF n <> 0 THEN
    RAISE EXCEPTION 'a service caller reads project_repositories directly (% rows); '
      'an inline query would appear to work and the policy has changed', n;
  END IF;

  SELECT system_repository_check('check-cell', 'checkrig') INTO got;
  IF got IS DISTINCT FROM 'npm ci --ignore-scripts && npm test' THEN
    RAISE EXCEPTION 'a node cannot read the check command for its rig (got %)', got;
  END IF;

  RAISE NOTICE 'a node can read its rig''s check command, through the function and not otherwise';
END $$;

RESET ROLE;

ROLLBACK;
