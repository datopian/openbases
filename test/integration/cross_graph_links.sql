-- Work that crosses graphs is linked in the control plane, under RLS (WP-D2).
--
-- Beads graphs are isolated and cannot reference each other (plan section 2.3),
-- so a company decision, a project epic and a personal follow-up living in
-- three different graphs can only be related here. work_links is that layer,
-- and WP-D2's first criterion is that the three are created and linked through
-- it.
--
-- It also had no row-level security at all until 0070 -- not enabled, not
-- forced, no policies. 0010 sweeps tables carrying a project_id column and an
-- edge carries none: the project lives on the work_refs it names. So an edge
-- was readable AND writable by anybody, including the 'evidences' edges that
-- are audit history.
--
-- The rule asserted here: you see an edge if you may see both beads it
-- connects. Not one, both -- an edge whose far end is a client's bead tells you
-- that client is involved, which is the leak that matters. Not the content of
-- the work, but who is working with whom.

BEGIN;

DO $$
DECLARE
  org uuid; node uuid; cell uuid;
  alpha uuid; beta uuid;
  member uuid; outsider uuid;
  hq uuid; g_alpha uuid; g_beta uuid; g_personal uuid;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Links member', 'test-links-member@example.invalid')
  RETURNING id INTO member;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Links outsider', 'test-links-outsider@example.invalid')
  RETURNING id INTO outsider;
  PERFORM set_config('test.member', member::text, false);
  PERFORM set_config('test.outsider', outsider::text, false);

  INSERT INTO execution_nodes (hostname, environment) VALUES ('test-links-node', 'staging');
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  SELECT id, 'test-links-cell', 'wgcell_test_links', 'client-restricted'
    FROM execution_nodes WHERE hostname = 'test-links-node'
  RETURNING id INTO cell;

  INSERT INTO projects (organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id, execution_cell_id)
  VALUES (org, 'test-links-alpha', 'Links alpha', 'confidential', member, outsider, NULL)
  RETURNING id INTO alpha;
  INSERT INTO projects (organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id, execution_cell_id)
  VALUES (org, 'test-links-beta', 'Links beta', 'restricted', outsider, member, cell)
  RETURNING id INTO beta;
  PERFORM set_config('test.alpha', alpha::text, false);
  PERFORM set_config('test.beta', beta::text, false);

  -- The member belongs to alpha and not to beta.
  INSERT INTO project_memberships (project_id, user_id, role_name)
  VALUES (alpha, member, 'contributor');

  -- Three graphs, which is the point: the beads below cannot reference each
  -- other from inside Beads, so the edges have to live here.
  INSERT INTO beads_databases (organisation_id, name, scope, project_id, path, host)
  VALUES (org, 'test-links-hq', 'company', NULL, '/srv/graphs/test-links-hq', 'test-control')
  RETURNING id INTO hq;
  INSERT INTO beads_databases (organisation_id, name, scope, project_id, path, host)
  VALUES (org, 'test-links-alpha-graph', 'project', alpha, '/srv/graphs/test-links-alpha', 'test-control')
  RETURNING id INTO g_alpha;
  INSERT INTO beads_databases (organisation_id, name, scope, project_id, path, host)
  VALUES (org, 'test-links-beta-graph', 'project', beta, '/srv/graphs/test-links-beta', 'test-control')
  RETURNING id INTO g_beta;
  -- A personal graph belongs to exactly one person and never to a project,
  -- which the schema enforces: personal_scope_has_owner.
  INSERT INTO beads_databases (organisation_id, name, scope, project_id, owner_user_id, path, host)
  VALUES (org, 'test-links-personal', 'personal', NULL, member,
          '/srv/graphs/test-links-personal', 'test-control')
  RETURNING id INTO g_personal;

  -- A company decision, a project epic, a personal follow-up, and one bead in
  -- a project the member cannot see.
  INSERT INTO work_refs (organisation_id, beads_database_id, bead_id, title, kind, status, visibility, project_id)
  VALUES (org, hq, 'wg-dec1', 'A company decision', 'decision', 'open', 'internal', NULL),
         (org, g_alpha, 'alp-epic', 'A project epic', 'epic', 'open', 'confidential', alpha),
         (org, g_personal, 'me-1', 'A personal follow-up', 'task', 'open', 'internal', NULL),
         (org, g_beta, 'bet-1', 'Work in a project the member cannot see', 'task', 'open', 'restricted', beta);
END $$;

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE
  n integer; dec uuid; epic uuid; personal uuid; hidden uuid; caught text;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.member'), true);

  SELECT id INTO dec FROM work_refs WHERE bead_id = 'wg-dec1';
  SELECT id INTO epic FROM work_refs WHERE bead_id = 'alp-epic';
  SELECT id INTO personal FROM work_refs WHERE bead_id = 'me-1';
  IF dec IS NULL OR epic IS NULL OR personal IS NULL THEN
    RAISE EXCEPTION 'the member cannot see the three beads they are entitled to';
  END IF;

  -- WP-D2 c1: the three are linked through work_links, across three graphs.
  INSERT INTO work_links (from_work_ref, to_work_ref, relation, created_by)
  VALUES (epic, dec, 'implements', current_setting('test.member')::uuid),
         (personal, epic, 'informs', current_setting('test.member')::uuid);

  SELECT count(*) INTO n FROM work_links
   WHERE from_work_ref IN (epic, personal);
  IF n <> 2 THEN
    RAISE EXCEPTION 'the member sees % of the 2 edges they created', n;
  END IF;

  -- The edges genuinely cross graphs, which is what Beads cannot express.
  SELECT count(DISTINCT w.beads_database_id) INTO n
    FROM work_links l JOIN work_refs w
      ON w.id IN (l.from_work_ref, l.to_work_ref)
   WHERE l.from_work_ref IN (epic, personal);
  IF n < 3 THEN
    RAISE EXCEPTION 'the edges span % graphs, so they prove nothing about crossing', n;
  END IF;

  -- An edge to a bead the member cannot see cannot be created: the far end
  -- would tell them a project they have no access to is involved.
  SELECT id INTO hidden FROM work_refs WHERE bead_id = 'bet-1';
  IF hidden IS NOT NULL THEN
    RAISE EXCEPTION 'the member can already see a restricted project''s bead';
  END IF;

  BEGIN
    INSERT INTO work_links (from_work_ref, to_work_ref, relation, created_by)
    SELECT epic, id, 'relates', current_setting('test.member')::uuid
      FROM work_refs WHERE bead_id = 'bet-1';
    -- The SELECT returns no row under RLS, so nothing is inserted rather than
    -- being refused. Asserted as a count, because "no rows inserted" and "the
    -- policy refused it" are different outcomes and only one of them is
    -- happening here.
    SELECT count(*) INTO n FROM work_links WHERE relation = 'relates';
    IF n <> 0 THEN
      RAISE EXCEPTION 'an edge to an invisible bead was created';
    END IF;
  EXCEPTION WHEN insufficient_privilege THEN
    NULL;
  END;

  RAISE NOTICE 'a company decision, a project epic and a personal follow-up are linked across three graphs';
END $$;

-- The outsider sees neither the edges nor the beads. Before 0070 they saw
-- every edge in the table.
DO $$
DECLARE n integer;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.outsider'), true);

  SELECT count(*) INTO n FROM work_links l
    JOIN work_refs w ON w.id = l.from_work_ref
   WHERE w.bead_id IN ('alp-epic', 'me-1');
  IF n <> 0 THEN
    RAISE EXCEPTION 'an outsider can read % cross-graph edges', n;
  END IF;

  -- Counted without the join too: the join could be doing the hiding on its
  -- own, which would leave work_links itself open and the test passing.
  SELECT count(*) INTO n FROM work_links WHERE relation IN ('implements', 'informs');
  IF n <> 0 THEN
    RAISE EXCEPTION 'an outsider reads % edges directly from work_links', n;
  END IF;

  RAISE NOTICE 'an outsider reads no cross-graph edge';
END $$;

-- An edge is audit history: 'evidences' and 'supersedes' are relations here.
-- Deleting one is tampering, and with no policy the table permitted it from
-- anybody.
DO $$
DECLARE n integer; before_count integer;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.outsider'), true);
  SELECT count(*) INTO before_count FROM work_links;

  DELETE FROM work_links WHERE relation IN ('implements', 'informs');

  PERFORM set_config('workgraph.user_id', current_setting('test.member'), true);
  SELECT count(*) INTO n FROM work_links WHERE relation IN ('implements', 'informs');
  IF n <> 2 THEN
    RAISE EXCEPTION 'an outsider deleted % of the 2 edges', 2 - n;
  END IF;

  RAISE NOTICE 'an outsider cannot delete an edge they cannot see';
END $$;

ROLLBACK;
