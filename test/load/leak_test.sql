-- WP-I3 criterion 1: no cross-project data leak, at load.
--
-- Runs against the seeded load database (50 projects, 105,000 work items). For
-- every user it compares what row-level security actually returns against what
-- the policy entitles them to, and fails on any difference.
--
-- Two things make this test worth trusting rather than merely green.
--
-- It runs as workgraph_app via SET ROLE, not as the owner. postgres is a
-- superuser and bypasses RLS entirely, so the same queries as postgres would
-- return everything and report no leak — the test would pass by being blind.
-- (wg-8yv.50 exists because the integration suite has this problem.)
--
-- It asserts POSITIVE visibility as well as the absence of leaks. A leak test
-- where every query returns zero rows passes trivially; that is the failure
-- mode of every isolation test that has ever given false comfort. So it also
-- requires that members see their own work items, and that an org-wide reader
-- sees across projects.

\set ON_ERROR_STOP on

-- ---------------------------------------------------------------------------
-- Ground truth, captured BEFORE dropping privileges
-- ---------------------------------------------------------------------------
-- This has to happen as the owner. project_memberships, role_grants and
-- projects are all under row-level security themselves, so building the
-- "what should this user see" baseline after SET ROLE filters the baseline as
-- well — and the test then compares RLS against RLS and calls every visible
-- project a leak.
--
-- That is exactly what the first version did. It reported 41 leaks across 10
-- users while announcing "checked 10 users against 0 projects", and the 0 is
-- what gave it away: the reference set was empty, not the data.
CREATE TEMP TABLE truth_projects AS
SELECT id AS project_id, slug, visibility FROM projects;

CREATE TEMP TABLE truth_memberships AS
SELECT user_id, project_id FROM project_memberships;

CREATE TEMP TABLE truth_grants AS
SELECT user_id FROM role_grants
 WHERE project_id IS NULL
   AND role_name IN ('organisation_admin', 'executive')
   AND (expires_at IS NULL OR expires_at > now());

CREATE TEMP TABLE truth_users AS
SELECT id, primary_email FROM users WHERE primary_email LIKE 'load-user-%';

CREATE TEMP TABLE truth_work AS
SELECT project_id, count(*) AS rows FROM work_refs
 WHERE project_id IS NOT NULL GROUP BY project_id;

-- Temp tables survive SET ROLE but are owned by the creating role, so the
-- reduced role needs read access to them explicitly.
GRANT SELECT ON truth_projects, truth_memberships, truth_grants,
                truth_users, truth_work TO workgraph_app;

SET ROLE workgraph_app;

DO $$
DECLARE
  u record;
  leaked_projects int;
  visible_projects int;
  entitled_projects int;
  checked int := 0;
  total_leaks int := 0;
  zero_visibility int := 0;
BEGIN
  IF (SELECT current_user) <> 'workgraph_app' THEN
    RAISE EXCEPTION 'running as %, not workgraph_app — RLS would be bypassed and this test would be meaningless',
      (SELECT current_user);
  END IF;

  -- A SAMPLE of users, not all sixty, and the reason is itself a finding.
  --
  -- The RLS predicate on work_refs calls can_read_project_row() — a SECURITY
  -- DEFINER function — once per row. At 105,000 rows that is 105,000 function
  -- calls for a single unfiltered listing, and sixty users took over ten
  -- minutes. Sampling keeps the assertion meaningful while the performance
  -- characteristic is measured separately by query_latency.sql.
  --
  -- The sample is deterministic (ordered by email, every sixth user) so a
  -- failure is reproducible, and it deliberately spans the membership stride
  -- used by the seed so it includes members and non-members of restricted
  -- projects.
  FOR u IN
    SELECT id, primary_email FROM (
      SELECT id, primary_email, row_number() OVER (ORDER BY primary_email) AS rn
        FROM truth_users
    ) t WHERE rn % 6 = 1
  LOOP
    PERFORM set_config('workgraph.user_id', u.id::text, true);

    -- Which projects RLS actually exposes to this user. Restricted to the
    -- projects in the fixture so the scan is bounded by the index rather than
    -- walking every row.
    CREATE TEMP TABLE seen AS
    SELECT DISTINCT w.project_id
      FROM work_refs w
     WHERE w.project_id IS NOT NULL
       AND w.status = 'open';

    SELECT count(*) INTO visible_projects FROM seen;

    SELECT count(*) INTO entitled_projects
      FROM truth_work tw
     WHERE EXISTS (SELECT 1 FROM truth_memberships m
                    WHERE m.project_id = tw.project_id AND m.user_id = u.id)
        OR EXISTS (SELECT 1 FROM truth_grants g WHERE g.user_id = u.id);

    -- A project this user can see and has no claim on.
    SELECT count(*) INTO leaked_projects
      FROM seen s
     WHERE NOT EXISTS (SELECT 1 FROM truth_memberships m
                        WHERE m.project_id = s.project_id AND m.user_id = u.id)
       AND NOT EXISTS (SELECT 1 FROM truth_grants g WHERE g.user_id = u.id);

    DROP TABLE seen;

    IF leaked_projects > 0 THEN
      total_leaks := total_leaks + leaked_projects;
      RAISE WARNING 'LEAK: % can read % project(s) they are not entitled to',
        u.primary_email, leaked_projects;
    END IF;

    IF visible_projects <> entitled_projects THEN
      RAISE WARNING 'MISMATCH: % sees % project(s), entitled to %',
        u.primary_email, visible_projects, entitled_projects;
      total_leaks := total_leaks + abs(visible_projects - entitled_projects);
    END IF;

    IF visible_projects = 0 THEN
      zero_visibility := zero_visibility + 1;
    END IF;

    checked := checked + 1;
  END LOOP;

  PERFORM set_config('workgraph.user_id', '', true);
  RAISE NOTICE 'checked % users against % projects', checked, (SELECT count(*) FROM truth_work);

  -- Refuse an empty population.
  --
  -- The anti-vacuity check below counts users who could see nothing, which is
  -- the right guard when there ARE users. It cannot catch the case where the
  -- loop never ran: with no users, zero_visibility stays 0 and everything
  -- downstream passes. Run against a database with no load fixture this printed
  -- "checked 0 users against 0 projects" and then "no cross-project leak", which
  -- is the most reassuring possible way to say nothing was tested.
  --
  -- Found by running this against the live staging database by mistake, where
  -- the fixture does not exist. The mistake was mine; the sentence it produced
  -- was the test's.
  IF checked = 0 THEN
    RAISE EXCEPTION 'no users were checked: this database has no load fixture, so '
                    'the assertions below would report success having tested nothing. '
                    'Apply test/load/seed.sql first.';
  END IF;

  IF total_leaks > 0 THEN
    RAISE EXCEPTION 'CROSS-PROJECT LEAK: % offending project(s) across % users', total_leaks, checked;
  END IF;

  -- The anti-vacuity check. Every load user is a member of at least one project
  -- by construction, so if they all see nothing then RLS is denying everything
  -- and the absence of leaks above means nothing.
  IF zero_visibility > 0 THEN
    RAISE EXCEPTION '% user(s) could see NO work items; this test would pass vacuously', zero_visibility;
  END IF;

  RAISE NOTICE 'no cross-project leak, and every user could see their own work';
END $$;

-- Restricted projects, specifically. These are the ones whose exposure would
-- matter most, so they are asserted separately rather than trusted to the
-- aggregate above.
DO $$
DECLARE
  restricted_total bigint;
  seen_by_outsider bigint;
  outsider uuid;
BEGIN
  SELECT sum(tw.rows) INTO restricted_total
    FROM truth_work tw JOIN truth_projects p ON p.project_id = tw.project_id
   WHERE p.visibility = 'restricted';

  IF restricted_total = 0 THEN
    RAISE EXCEPTION 'no restricted work items in the fixture; this assertion would prove nothing';
  END IF;

  -- A user who is a member of no restricted project at all.
  SELECT u.id INTO outsider
    FROM truth_users u
   WHERE NOT EXISTS (
       SELECT 1 FROM truth_memberships m
         JOIN truth_projects p ON p.project_id = m.project_id
        WHERE m.user_id = u.id AND p.visibility = 'restricted')
   LIMIT 1;

  IF outsider IS NULL THEN
    RAISE EXCEPTION 'every user is a member of some restricted project; cannot test an outsider';
  END IF;

  PERFORM set_config('workgraph.user_id', outsider::text, true);
  SELECT count(*) INTO seen_by_outsider
    FROM work_refs w
     JOIN truth_projects p ON p.project_id = w.project_id
   WHERE p.visibility = 'restricted';
  PERFORM set_config('workgraph.user_id', '', true);

  IF seen_by_outsider <> 0 THEN
    RAISE EXCEPTION 'an outsider can read % of % restricted work items', seen_by_outsider, restricted_total;
  END IF;

  RAISE NOTICE 'restricted work items (%) invisible to a non-member', restricted_total;
END $$;

RESET ROLE;
