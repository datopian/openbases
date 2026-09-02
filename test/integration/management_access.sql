-- Company management reads everything, and that is a decision (wg-8yv.39).
--
-- organisation_admin and executive are granted company-wide -- project_id NULL
-- in role_grants -- so can_read_project honours them on every project,
-- client-restricted ones included. Four people hold one or both.
--
-- This asserts it deliberately, for three reasons.
--
-- It is invisible from any single policy. can_read_project mentions the roles
-- once, and every table that delegates to it inherits the reach without saying
-- so; a reader of knowledge_records' policy cannot tell that four people are
-- outside its project filter.
--
-- It surprised the tests that came before it. Two of them tried to use a real
-- person as somebody who should NOT see a project, and both passed on a fresh
-- database for the wrong reason and failed against staging -- every candidate
-- outsider turned out to be management. Both now create a user with no grants.
--
-- And it is the kind of access somebody tightens as an obvious improvement. A
-- test that fails when the grants are scoped per project says which decision
-- is being reversed, rather than letting the reversal look like a fix.
--
-- Record mem-003b900b-4cb6-432b-af96-c0f504ea3559 is the accepted decision.

BEGIN;

DO $$
DECLARE org uuid; n integer;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  -- A restricted project nobody in management is a member of, to be sure the
  -- reach comes from the role grant rather than from a membership. CDT and
  -- NGED both have management as owner or backup, which would confuse the two.
  --
  -- Everything it needs is created here rather than borrowed. A restricted
  -- project must have its own execution cell (plan section 7.4), and CI's
  -- database has no cells and no registered graphs -- those arrive at deploy
  -- from the registry binary, not from a migration. Borrowing them passed on
  -- staging and failed in CI on a CHECK about a null cell, which reads like a
  -- schema problem rather than like missing fixture data.
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Management access test owner', 'test-mgmt-owner@example.invalid'),
         (org, 'Management access test backup', 'test-mgmt-backup@example.invalid');

  INSERT INTO execution_nodes (hostname, environment)
  VALUES ('test-mgmt-node', 'staging');

  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  SELECT id, 'test-mgmt-cell', 'wgcell_test_mgmt', 'client-restricted'
    FROM execution_nodes WHERE hostname = 'test-mgmt-node';

  INSERT INTO projects (organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id, execution_cell_id)
  SELECT org, 'test-mgmt-client', 'Management access test', 'restricted',
         (SELECT id FROM users WHERE primary_email = 'test-mgmt-owner@example.invalid'),
         (SELECT id FROM users WHERE primary_email = 'test-mgmt-backup@example.invalid'),
         (SELECT id FROM execution_cells WHERE slug = 'test-mgmt-cell');

  INSERT INTO knowledge_sources (organisation_id, provider, provider_source_id,
                                 provider_revision, source_type, project_id,
                                 visibility, captured_at)
  SELECT org, 'google-meet', 'test/management-access', 'sha256:mgmt', 'transcript',
         (SELECT id FROM projects WHERE slug = 'test-mgmt-client'), 'restricted', now();

  -- The project's graph, which carries a name, an absolute path and a host --
  -- client information in its own right.
  INSERT INTO beads_databases (organisation_id, name, scope, project_id, path, host)
  SELECT org, 'test-mgmt-graph', 'project',
         (SELECT id FROM projects WHERE slug = 'test-mgmt-client'),
         '/srv/graphs/test-mgmt', 'test-control';

  -- Somebody with no grants and no memberships, as the control. Without this
  -- the test would pass on a database where RLS was switched off entirely.
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Management access test outsider', 'test-mgmt-outsider@example.invalid');

  -- Resolved HERE, as the owner, and stashed. role_grants is itself behind
  -- row-level security, so the loop below -- which runs as workgraph_app while
  -- switching identities -- reads an empty table and finds nobody to check.
  -- That failed as "checked 0 management role holders", which reads like the
  -- grants are missing rather than like the query cannot see them.
  PERFORM set_config('test.management', (
    SELECT string_agg(DISTINCT u.id::text, ',' ORDER BY u.id::text)
      FROM role_grants g
      JOIN users u ON u.id = g.user_id
     WHERE g.project_id IS NULL
       AND g.role_name IN ('organisation_admin', 'executive')
       AND (g.expires_at IS NULL OR g.expires_at > now())), false);

  IF coalesce(current_setting('test.management', true), '') = '' THEN
    RAISE EXCEPTION 'no company-wide management grants exist; this test asserts nothing';
  END IF;
END $$;

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE who uuid; label text; n integer; seen integer := 0;
BEGIN
  -- Every holder of a live company-wide management grant, by grant rather than
  -- by name. Naming the four people would assert an org chart; reading the
  -- grants asserts the policy, and a fifth appointment is covered the day it
  -- is made.
  FOREACH label IN ARRAY string_to_array(current_setting('test.management'), ',')
  LOOP
    who := label::uuid;
    seen := seen + 1;
    PERFORM set_config('workgraph.user_id', label, true);
    -- Their own row is readable to them, so the message can name a person.
    SELECT primary_email INTO label FROM users WHERE id = who;
    label := coalesce(label, who::text);

    -- The project itself.
    SELECT count(*) INTO n FROM projects WHERE slug = 'test-mgmt-client';
    IF n <> 1 THEN
      RAISE EXCEPTION '% cannot see a restricted project they are not a member of', label;
    END IF;

    -- And its material. The project row is the easy half; the point of the
    -- grant is that the transcripts, candidates and records follow.
    SELECT count(*) INTO n FROM knowledge_sources
     WHERE provider_source_id = 'test/management-access';
    IF n <> 1 THEN
      RAISE EXCEPTION '% cannot see a restricted project''s sources', label;
    END IF;

    -- The work graph too: a graph name and path is client information.
    SELECT count(*) INTO n FROM beads_databases WHERE name = 'test-mgmt-graph';
    IF n <> 1 THEN
      RAISE EXCEPTION '% cannot see a client project''s graph', label;
    END IF;
  END LOOP;

  -- Every holder found is checked, and the block above refuses to run with
  -- none. No count assertion: "expected at least 4" was an org-chart claim,
  -- and it failed in CI where the seed had two -- three of the six grants
  -- existed only on staging until 0066 put them in a migration. The property
  -- is "each holder reads everything", and a fifth appointment should not
  -- break a test about a policy.
  IF seen = 0 THEN
    RAISE EXCEPTION 'no management role holders were checked';
  END IF;

  RAISE NOTICE 'all % company-wide role holders read restricted client material', seen;
END $$;

-- The control. If this passes too, the test above proved nothing about roles.
DO $$
DECLARE outsider uuid; n integer;
BEGIN
  SELECT id INTO outsider FROM users
   WHERE primary_email = 'test-mgmt-outsider@example.invalid';
  PERFORM set_config('workgraph.user_id', outsider::text, true);

  SELECT count(*) INTO n FROM projects WHERE slug = 'test-mgmt-client';
  IF n <> 0 THEN
    RAISE EXCEPTION 'somebody with no grants can see a restricted project';
  END IF;
  SELECT count(*) INTO n FROM knowledge_sources
   WHERE provider_source_id = 'test/management-access';
  IF n <> 0 THEN
    RAISE EXCEPTION 'somebody with no grants can see a restricted project''s sources';
  END IF;
  SELECT count(*) INTO n FROM beads_databases WHERE name = 'test-mgmt-graph';
  IF n <> 0 THEN
    RAISE EXCEPTION 'somebody with no grants can see a client project''s graph';
  END IF;

  RAISE NOTICE 'somebody with no grants reads none of it';
END $$;

ROLLBACK;
