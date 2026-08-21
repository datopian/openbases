-- Chief-of-staff scoping (WP-F3).
--
-- The acceptance criterion is that restricted project data never appears for an
-- unauthorised user. This asserts it where it is actually enforced: the queries
-- run under the caller's identity, so row-level security decides what is
-- fetched at all. Data a user may not see is never read, and therefore cannot
-- reach an answer — or a model's context later.
--
-- Asserted as workgraph_app, because the owner bypasses RLS and would pass
-- every line below while the application saw something different.

BEGIN;

DO $$
DECLARE restricted uuid; outsider uuid; n integer;
BEGIN
  SELECT id INTO restricted FROM projects WHERE slug = 'nged';
  IF restricted IS NULL THEN
    RAISE NOTICE 'no restricted project in the seed; skipping';
    RETURN;
  END IF;

  -- Somebody who is NOT on the restricted engagement and holds no
  -- organisation-wide role. Both exclusions matter: an admin legitimately sees
  -- everything, so testing with one would prove nothing.
  SELECT m.user_id INTO outsider
    FROM project_memberships m
    JOIN projects p ON p.id = m.project_id
   WHERE p.slug <> 'nged'
     AND m.user_id NOT IN (
       SELECT m2.user_id FROM project_memberships m2
         JOIN projects p2 ON p2.id = m2.project_id WHERE p2.slug = 'nged')
     AND m.user_id NOT IN (
       SELECT g.user_id FROM role_grants g
        WHERE g.project_id IS NULL
          AND g.role_name IN ('organisation_admin', 'executive')
          AND (g.expires_at IS NULL OR g.expires_at > now()))
   LIMIT 1;

  IF outsider IS NULL THEN
    RAISE NOTICE 'no suitable outsider in the seed; skipping';
    RETURN;
  END IF;

  -- Give the restricted project something worth leaking.
  INSERT INTO pull_request_projections (repository_id, number, title, state, updated_at)
  SELECT r.id, 9911, 'Confidential client change', 'open', now()
    FROM project_repositories r WHERE r.project_id = restricted LIMIT 1;

  PERFORM set_config('workgraph.user_id', outsider::text, true);
  SET LOCAL ROLE workgraph_app;
  -- Same guard as test/integration/assert_app_role.sql, inline because a psql
  -- include cannot appear inside a PL/pgSQL block.
  IF current_user <> 'workgraph_app' THEN
    RAISE EXCEPTION 'running as %, not workgraph_app: row-level security is bypassed and the assertions below would pass regardless of the policies', current_user;
  END IF;

  -- "What changed since yesterday?" must not surface it.
  SELECT count(*) INTO n
    FROM pull_request_projections pr
    JOIN project_repositories r ON r.id = pr.repository_id
    JOIN projects p ON p.id = r.project_id
   WHERE pr.updated_at >= now() - interval '1 day';
  IF n > 0 THEN
    PERFORM 1 FROM pull_request_projections pr
      JOIN project_repositories r ON r.id = pr.repository_id
     WHERE r.project_id = restricted AND pr.number = 9911;
    IF FOUND THEN
      RAISE EXCEPTION 'the restricted pull request is visible to an outsider';
    END IF;
  END IF;

  -- Even the project row must not appear in the cross-project answer.
  SELECT count(*) INTO n FROM projects WHERE id = restricted;
  IF n <> 0 THEN
    RAISE EXCEPTION 'the restricted project itself is visible to an outsider';
  END IF;

  -- Nor its repositories, whose names alone identify the client.
  SELECT count(*) INTO n FROM project_repositories WHERE project_id = restricted;
  IF n <> 0 THEN
    RAISE EXCEPTION 'restricted repositories are visible to an outsider';
  END IF;

  RESET ROLE;
  RAISE NOTICE 'chief-of-staff scoping assertions passed';
END
$$;

ROLLBACK;
