-- A newly created project is visible to the people responsible for it (wg-1dm).
--
-- This is the invariant behind "I created a project and could not see it".
--
-- can_read_project() grants access two ways: a project_memberships row, or an
-- organisation-wide organisation_admin or executive grant. Nothing else. So a
-- project inserted with owners but WITHOUT membership rows is invisible to both
-- of its owners unless one of them happens to be an administrator -- and on
-- staging the four people who hold those grants would never notice, because
-- they see every project regardless.
--
-- That is why CreateProject writes both memberships, and why this test uses
-- owners with no role grants at all: with a real administrator as owner the
-- test would pass whether the memberships were written or not.

BEGIN;

DO $$
DECLARE
  org      uuid;
  owner_id uuid;
  backup   uuid;
  bystander uuid;
  proj     uuid;
  n        integer;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  -- Three users with NO grants. Every "outsider" in earlier tests turned out
  -- to hold a company-wide grant, which made the assertion vacuous.
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Owner With No Grants', 'owner-nogrants@example.invalid')
  RETURNING id INTO owner_id;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Backup With No Grants', 'backup-nogrants@example.invalid')
  RETURNING id INTO backup;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Unrelated Person', 'bystander-nogrants@example.invalid')
  RETURNING id INTO bystander;

  -- Exactly what CreateProject does: the row, then both memberships.
  INSERT INTO projects
      (organisation_id, slug, name, visibility, primary_owner_id, backup_owner_id)
  VALUES (org, 'creation-visibility-probe', 'Creation Visibility Probe',
          'internal', owner_id, backup)
  RETURNING id INTO proj;

  -- role_name, and the values must exist in the roles table, which this
  -- references. can_read_project() looks only for a membership row and not at
  -- which role it names, so any valid role grants visibility -- but a wrong
  -- column name or an unknown role is a hard failure, which is how the first
  -- draft of this test caught the same mistake in CreateProject.
  INSERT INTO project_memberships (project_id, user_id, role_name)
  VALUES (proj, owner_id, 'project_lead'), (proj, backup, 'backup_operator');

  -- One attached repository, and the proof that a second attach of the same
  -- repository is a conflict rather than a move. Written as the owner, before
  -- privileges are dropped, because this asserts the constraint rather than
  -- who may write.
  INSERT INTO project_repositories (project_id, provider, owner, name)
  VALUES (proj, 'github', 'datopian', 'creation-visibility-probe-repo');

  BEGIN
    INSERT INTO project_repositories (project_id, provider, owner, name)
    VALUES (proj, 'github', 'datopian', 'creation-visibility-probe-repo');
    RAISE EXCEPTION 'a repository was attached twice; UNIQUE (provider, owner, name) is gone';
  EXCEPTION WHEN unique_violation THEN
    -- Expected: 23505, which is what isUniqueViolation matches on, and what
    -- AttachRepositories turns into a per-repository "taken" result instead of
    -- failing the whole batch.
    NULL;
  END;

  -- Drop to the role the application actually uses. CI connects as postgres, a
  -- superuser, and a superuser never evaluates row-level security -- FORCE ROW
  -- LEVEL SECURITY does not change that. Without this the three assertions
  -- below would pass whether the policies were right, wrong or absent.
  SET LOCAL ROLE workgraph_app;

  IF current_user <> 'workgraph_app' THEN
    RAISE EXCEPTION 'running as %, not workgraph_app: row-level security is bypassed and the assertions below would pass regardless of the policies', current_user;
  END IF;

  -- The primary owner sees it.
  PERFORM set_config('workgraph.user_id', owner_id::text, true);
  SELECT count(*) INTO n FROM projects WHERE id = proj;
  IF n <> 1 THEN
    RAISE EXCEPTION 'the primary owner cannot see the project they own (found %)', n;
  END IF;

  -- So does the backup owner. Asserted separately: one membership row would
  -- satisfy the check above and still leave the backup owner blind, which is
  -- the half somebody would drop as redundant.
  PERFORM set_config('workgraph.user_id', backup::text, true);
  SELECT count(*) INTO n FROM projects WHERE id = proj;
  IF n <> 1 THEN
    RAISE EXCEPTION 'the backup owner cannot see the project they are responsible for (found %)', n;
  END IF;

  -- And somebody unrelated does not.
  --
  -- This assertion is also the proof that the SET LOCAL ROLE above took
  -- effect: as a superuser this count would be 1, and the test would fail
  -- here rather than passing for the wrong reason.
  PERFORM set_config('workgraph.user_id', bystander::text, true);
  SELECT count(*) INTO n FROM projects WHERE id = proj;
  IF n <> 0 THEN
    RAISE EXCEPTION 'an unrelated user can see the project, so the policy is not being evaluated (found %)', n;
  END IF;

  -- The repositories the owner may see follow the project, not the row.
  PERFORM set_config('workgraph.user_id', owner_id::text, true);
  SELECT count(*) INTO n FROM project_repositories WHERE project_id = proj;
  IF n <> 1 THEN
    RAISE EXCEPTION 'the owner should see the one attached repository (found %)', n;
  END IF;

  PERFORM set_config('workgraph.user_id', bystander::text, true);
  SELECT count(*) INTO n FROM project_repositories WHERE project_id = proj;
  IF n <> 0 THEN
    RAISE EXCEPTION 'an unrelated user can see the project repositories (found %)', n;
  END IF;

  RESET ROLE;

  RAISE NOTICE 'project creation visibility: owner, backup owner and conflict all behave';
END $$;

ROLLBACK;
