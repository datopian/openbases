-- A plan is filed into a project the requester belongs to, or not at all (B2).
--
-- system_enqueue_work took a cell and no project, so every plan job and every
-- bead it produced landed with no project. That is company-wide work under
-- 0071 -- readable by everybody who can log in -- so planning a client
-- engagement through this path published the brief and its beads to the whole
-- company.
--
-- The check is on the REQUESTER, not on a project name the caller asserts.

BEGIN;

DO $$
DECLARE org uuid; node uuid; cell uuid; alpha uuid; beta uuid;
        member uuid; outsider uuid; boss uuid;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Plan member', 'test-pl-member@example.invalid') RETURNING id INTO member;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Plan outsider', 'test-pl-outsider@example.invalid') RETURNING id INTO outsider;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Plan manager', 'test-pl-manager@example.invalid') RETURNING id INTO boss;
  INSERT INTO role_grants (user_id, role_name, organisation_id)
  VALUES (boss, 'executive', org);

  PERFORM set_config('test.member', member::text, false);
  PERFORM set_config('test.outsider', outsider::text, false);
  PERFORM set_config('test.boss', boss::text, false);

  INSERT INTO execution_nodes (hostname, environment) VALUES ('test-pl-node', 'staging');
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  SELECT id, 'test-pl-cell', 'wgcell_test_pl', 'oss'
    FROM execution_nodes WHERE hostname = 'test-pl-node' RETURNING id INTO cell;

  INSERT INTO projects (organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id, execution_cell_id)
  VALUES (org, 'test-pl-alpha', 'Plan alpha', 'internal', member, outsider, cell)
  RETURNING id INTO alpha;
  INSERT INTO projects (organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id, execution_cell_id)
  VALUES (org, 'test-pl-beta', 'Plan beta', 'internal', outsider, member, cell)
  RETURNING id INTO beta;

  INSERT INTO project_memberships (project_id, user_id, role_name)
  VALUES (alpha, member, 'contributor');
END $$;

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE job uuid; caught text; n integer; claimed record;
BEGIN
  -- The identity is for the VERIFICATION queries below, not for the enqueue:
  -- system_enqueue_work is SECURITY DEFINER and takes the requester as a
  -- parameter, so it works either way -- but `projects` is behind RLS, and
  -- joining it with no identity set returns nothing, which reads as "the job
  -- was not filed" rather than as "this query cannot see the project".
  PERFORM set_config('workgraph.user_id', current_setting('test.member'), true);

  -- A member files into their own project.
  job := system_enqueue_work('plan', 'test-pl-cell', 'sandbox', NULL,
                             'A brief for alpha', current_setting('test.member')::uuid,
                             'test-pl-alpha');
  SELECT count(*) INTO n FROM work_queue q
    JOIN projects p ON p.id = q.project_id
   WHERE q.id = job AND p.slug = 'test-pl-alpha';
  IF n <> 1 THEN RAISE EXCEPTION 'the job was not filed into alpha'; END IF;

  -- A non-member cannot, even naming a project that exists. This is the whole
  -- point: without it, anybody could file work into a client's project and
  -- then read it back, because 0071 makes the queue readable through the
  -- project.
  BEGIN
    PERFORM system_enqueue_work('plan', 'test-pl-cell', 'sandbox', NULL,
                                'A brief for beta', current_setting('test.member')::uuid,
                                'test-pl-beta');
    caught := '(nothing raised)';
  EXCEPTION WHEN others THEN
    caught := sqlerrm;
  END;
  IF caught NOT LIKE '%not a member of project%' THEN
    RAISE EXCEPTION 'filing into a project the requester is not in reported: %', caught;
  END IF;

  -- A project that does not exist is refused rather than silently ignored.
  -- Ignoring it would file the work company-wide, which is the widening this
  -- exists to prevent.
  BEGIN
    PERFORM system_enqueue_work('plan', 'test-pl-cell', 'sandbox', NULL,
                                'A brief', current_setting('test.member')::uuid,
                                'no-such-project');
    caught := '(nothing raised)';
  EXCEPTION WHEN others THEN
    caught := sqlerrm;
  END;
  IF caught NOT LIKE '%no project with the slug%' THEN
    RAISE EXCEPTION 'an unknown project reported: %', caught;
  END IF;

  -- Company management files anywhere, consistent with reading everywhere.
  -- The identity switches too, so the verification join can see beta.
  PERFORM set_config('workgraph.user_id', current_setting('test.boss'), true);
  job := system_enqueue_work('plan', 'test-pl-cell', 'sandbox', NULL,
                             'A brief for beta', current_setting('test.boss')::uuid,
                             'test-pl-beta');
  SELECT count(*) INTO n FROM work_queue q
    JOIN projects p ON p.id = q.project_id
   WHERE q.id = job AND p.slug = 'test-pl-beta';
  IF n <> 1 THEN RAISE EXCEPTION 'management could not file into beta'; END IF;

  PERFORM set_config('workgraph.user_id', current_setting('test.member'), true);

  -- No project at all still works: company-wide work is a real case, not an
  -- accident, and the six-argument call has to keep resolving.
  job := system_enqueue_work('plan', 'test-pl-cell', 'sandbox', NULL,
                             'Company work', current_setting('test.member')::uuid);
  SELECT count(*) INTO n FROM work_queue WHERE id = job AND project_id IS NULL;
  IF n <> 1 THEN RAISE EXCEPTION 'the six-argument call did not file company work'; END IF;

  RAISE NOTICE 'a plan is filed into a project the requester belongs to, or not at all';
END $$;

-- The claim carries the project to the runner, so the planning agent can label
-- the beads it creates and the dispatcher can stamp work_refs.
DO $$
DECLARE claimed record; n integer;
BEGIN
  SELECT * INTO claimed FROM system_claim_work('test-pl-cell');
  IF claimed.id IS NULL THEN
    RAISE EXCEPTION 'nothing was claimable';
  END IF;
  -- The oldest queued job is the member's alpha plan.
  IF claimed.project IS DISTINCT FROM 'test-pl-alpha' THEN
    RAISE EXCEPTION 'the claim reported project %', coalesce(claimed.project, '(none)');
  END IF;

  RAISE NOTICE 'the claim carries the project to the runner';
END $$;

-- And the queue overview shows it, filtered as 0071 requires: a client's plan
-- job is visible to the client's team, which needs the job's OWN project and
-- not just its bead's -- a plan job has no bead.
DO $$
DECLARE n integer;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.member'), true);
  SELECT count(*) INTO n FROM system_queue_overview(NULL)
   WHERE project = 'test-pl-alpha';
  IF n <> 1 THEN RAISE EXCEPTION 'a member sees % of their project''s plan jobs', n; END IF;

  PERFORM set_config('workgraph.user_id', current_setting('test.outsider'), true);
  SELECT count(*) INTO n FROM system_queue_overview(NULL)
   WHERE project = 'test-pl-alpha';
  IF n <> 0 THEN RAISE EXCEPTION 'an outsider reads alpha''s plan job'; END IF;

  RAISE NOTICE 'the queue overview shows the project, filtered by membership';
END $$;

ROLLBACK;
