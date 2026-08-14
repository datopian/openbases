-- Pull request and CI projections (WP-D1).
--
-- The property under test is the one that corrupts silently: GitHub does not
-- guarantee delivery order, and a retry of an older event can arrive after a
-- newer one. Without a monotonic guard a redelivered "opened" reopens a merged
-- pull request — and redelivery is exactly what happens after an outage, so the
-- corruption lands precisely when the projection is already behind.

BEGIN;

-- The first block runs as the owner and asserts CONSTRAINTS: the ordering
-- guard, the CI state domain, the foreign key. Those are properties of the
-- schema and are the same for every role.
--
-- The second block runs as the APPLICATION role and asserts the SYSTEM PATH.
-- That separation matters: the owner bypasses row-level security, so a
-- projection that could never resolve a repository as workgraph_app passed
-- every assertion as postgres and reached staging looking healthy (wg-8yv.50).

DO $$
DECLARE repo uuid; st text; ck text; n integer;
BEGIN
  SELECT id INTO repo FROM project_repositories WHERE owner = 'datopian' AND name = 'portaljs';
  IF repo IS NULL THEN RAISE EXCEPTION 'pilot repository missing from the seed'; END IF;

  -- A pull request merges.
  INSERT INTO pull_request_projections
      (repository_id, number, title, state, author, head_sha, merged_at, updated_at)
  VALUES (repo, 4242, 'Add the thing', 'merged', 'someone', 'cafe1234',
          '2026-08-14T12:00:00Z', '2026-08-14T12:00:00Z');

  -- An OLDER event is redelivered afterwards. This is the guard.
  INSERT INTO pull_request_projections
      (repository_id, number, title, state, author, head_sha, merged_at, updated_at)
  VALUES (repo, 4242, 'Add the thing', 'open', 'someone', 'cafe1234',
          NULL, '2026-08-14T11:00:00Z')
  ON CONFLICT (repository_id, number) DO UPDATE
      SET state = EXCLUDED.state, merged_at = EXCLUDED.merged_at,
          updated_at = EXCLUDED.updated_at
      WHERE pull_request_projections.updated_at < EXCLUDED.updated_at;

  SELECT state INTO st FROM pull_request_projections
   WHERE repository_id = repo AND number = 4242;
  IF st <> 'merged' THEN
    RAISE EXCEPTION 'a stale redelivery reopened a merged pull request (state=%)', st;
  END IF;

  -- A genuinely newer event must still apply, or the guard would freeze the
  -- projection at its first value.
  INSERT INTO pull_request_projections
      (repository_id, number, title, state, author, head_sha, merged_at, updated_at)
  VALUES (repo, 4242, 'Add the thing, revised', 'merged', 'someone', 'cafe1234',
          '2026-08-14T12:00:00Z', '2026-08-14T13:00:00Z')
  ON CONFLICT (repository_id, number) DO UPDATE
      SET title = EXCLUDED.title, updated_at = EXCLUDED.updated_at
      WHERE pull_request_projections.updated_at < EXCLUDED.updated_at;

  SELECT title INTO st FROM pull_request_projections
   WHERE repository_id = repo AND number = 4242;
  IF st <> 'Add the thing, revised' THEN
    RAISE EXCEPTION 'the guard also blocked a newer event (title=%)', st;
  END IF;

  -- CI state is matched on the commit, not the pull request number: after a
  -- force-push an older suite is no longer about the code under review.
  UPDATE pull_request_projections p SET checks_state = 'success'
    FROM project_repositories r
   WHERE p.repository_id = r.id AND r.owner = 'datopian' AND r.name = 'portaljs'
     AND p.head_sha = 'cafe1234';

  SELECT checks_state INTO ck FROM pull_request_projections
   WHERE repository_id = repo AND number = 4242;
  IF ck <> 'success' THEN RAISE EXCEPTION 'CI state did not project (%)', ck; END IF;

  -- A suite for a commit nobody is reviewing must match nothing rather than
  -- landing on whatever row happens to share the pull request number.
  UPDATE pull_request_projections p SET checks_state = 'failure'
    FROM project_repositories r
   WHERE p.repository_id = r.id AND r.owner = 'datopian' AND r.name = 'portaljs'
     AND p.head_sha = 'deadbeef';
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 0 THEN RAISE EXCEPTION 'a suite for an unknown commit updated % row(s)', n; END IF;

  -- Only the four defined CI states are accepted.
  BEGIN
    UPDATE pull_request_projections SET checks_state = 'green'
     WHERE repository_id = repo AND number = 4242;
    RAISE EXCEPTION 'an undefined checks_state was accepted';
  EXCEPTION WHEN check_violation THEN NULL;
  END;

  -- A projection cannot exist without a registered repository, which is what
  -- ties every row to a project and therefore to a visibility class.
  BEGIN
    INSERT INTO pull_request_projections
        (repository_id, number, title, state, updated_at)
    VALUES ('00000000-0000-0000-0000-000000000000', 1, 'orphan', 'open', now());
    RAISE EXCEPTION 'a projection was accepted for an unregistered repository';
  EXCEPTION WHEN foreign_key_violation THEN NULL;
  END;

  RAISE NOTICE 'projection assertions passed';
END
$$;

-- As the application role, with NO user identity set — precisely the webhook's
-- situation, and the one that was broken.
SET LOCAL ROLE workgraph_app;

DO $$
BEGIN
  -- Through the SECURITY DEFINER function, as the application role, with no
  -- user identity set — which is precisely the webhook's situation.
  IF project_pull_request('123456', 'datopian', 'portaljs', 4243, 'Via the function',
                          'open', 'someone', 'beef5678', NULL, '2026-08-14T12:00:00Z') IS NOT TRUE THEN
    RAISE EXCEPTION 'the projection function could not resolve a registered repository';
  END IF;

  -- A stale event through the same function must not apply.
  IF project_pull_request('123456', 'datopian', 'portaljs', 4243, 'Stale',
                          'open', 'someone', 'beef5678', NULL, '2026-08-14T11:00:00Z') IS NOT FALSE THEN
    RAISE EXCEPTION 'the projection function applied a stale event';
  END IF;

  -- An unregistered repository yields NULL, distinct from false: nothing to
  -- project against, rather than nothing to do.
  IF project_pull_request('999999', 'someone', 'unregistered', 1, 'x',
                          'open', 'y', 'z', NULL, now()) IS NOT NULL THEN
    RAISE EXCEPTION 'an unregistered repository did not report as unknown';
  END IF;

  RAISE NOTICE 'system path assertions passed as workgraph_app';
END
$$;

RESET ROLE;

ROLLBACK;
