-- Creating a project does not require an organisation-wide grant.
--
-- Reported by a colleague who could not create a project on two separate days
-- and got "internal server error". The cause was not permission to create --
-- projects_insert is only current_app_user() IS NOT NULL -- but RETURNING.
--
-- Postgres applies the SELECT policy to a row returned by INSERT ... RETURNING,
-- and projects_read is can_read_project(id), which grants access through
-- membership or an org-wide admin/executive grant. The creator's membership is
-- inserted AFTER the project row, so at RETURNING time the project they just
-- created was not yet visible to them and the insert failed with "new row
-- violates row-level security policy for table projects" -- which names the
-- wrong policy.
--
-- It worked for the three people holding an org-wide grant, because
-- can_read_project lets those read anything. So it failed for everybody else
-- and worked for exactly the people who would have investigated it.
--
-- This test therefore runs as a user with NO grants and NO memberships, which
-- is the only lens that can see it.
--
-- Owner-only is NOT recorded for this file: it drops to workgraph_app and
-- asserts it took effect, because running as the owner would bypass RLS and
-- the test would pass with the bug reinstated.
\set ON_ERROR_STOP on

BEGIN;

DO $$
DECLARE
  org uuid; nobody uuid; other uuid; pid uuid; n integer;
BEGIN
  SELECT id INTO org FROM organisations ORDER BY created_at LIMIT 1;

  -- Deliberately ungranted and unmembered: the reporter's position exactly.
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'No Grants', 'no-grants@example.invalid') RETURNING id INTO nobody;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Some Backup', 'some-backup@example.invalid') RETURNING id INTO other;

  SELECT count(*) INTO n FROM role_grants WHERE user_id = nobody;
  IF n <> 0 THEN
      RAISE EXCEPTION 'the fixture user has % grant(s); the test would pass for the wrong reason', n;
  END IF;

  SET LOCAL ROLE workgraph_app;
  IF current_user <> 'workgraph_app' THEN
      RAISE EXCEPTION 'expected to be workgraph_app, am %', current_user;
  END IF;
  PERFORM set_config('workgraph.user_id', nobody::text, true);
  IF current_app_user() <> nobody THEN
      RAISE EXCEPTION 'the session identity did not take effect';
  END IF;

  -- The id is chosen first and the insert does not return the row, which is
  -- what CreateProject does now.
  pid := gen_random_uuid();
  INSERT INTO projects (id, organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id)
  VALUES (pid, org, 'anyone-can-create', 'Anyone Can Create', 'internal', nobody, other);

  -- Not readable yet, and that is the point: this is the state RETURNING
  -- would have been evaluated in.
  IF can_read_project(pid) THEN
      RAISE EXCEPTION 'the new project is readable before its membership exists, so this '
          'test can no longer detect the bug it was written for';
  END IF;

  -- The memberships that follow are what make it readable.
  INSERT INTO project_memberships (project_id, user_id, role_name)
  VALUES (pid, nobody, 'project_lead'), (pid, other, 'backup_operator');

  IF NOT can_read_project(pid) THEN
      RAISE EXCEPTION 'the creator still cannot read their own project after being made a member';
  END IF;

  -- And the whole reason the bug was invisible: with RETURNING it fails here.
  BEGIN
      INSERT INTO projects (organisation_id, slug, name, visibility,
                            primary_owner_id, backup_owner_id)
      VALUES (org, 'anyone-can-create-2', 'Second', 'internal', nobody, other)
      RETURNING id INTO pid;
      RAISE EXCEPTION 'INSERT ... RETURNING succeeded for an ungranted user; the read '
          'policy must have been widened, so this test no longer describes the system';
  EXCEPTION
      WHEN insufficient_privilege THEN
          NULL; -- expected: this is the failure the fix avoids
  END;
END
$$;

ROLLBACK;

SELECT 'a user with no grants can create a project' AS result;
