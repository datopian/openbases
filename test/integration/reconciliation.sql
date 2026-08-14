-- Projection visibility and the system path (WP-D3).
--
-- Two things are asserted here, and both were live defects:
--
--   1. pull_request_projections had no row-level security, so pull request
--      titles from the restricted client repository were readable by any
--      application query.
--
--   2. The reconciler read project_repositories directly and, having no user
--      identity, saw nothing. The pass reported zero repositories and zero
--      failures — indistinguishable from a healthy run.

BEGIN;

DO $$
DECLARE repo uuid; restricted uuid; n integer;
BEGIN
  SELECT id INTO repo FROM project_repositories WHERE owner='datopian' AND name='portaljs';
  SELECT id INTO restricted FROM project_repositories WHERE owner='datopian' AND name='nged';
  IF repo IS NULL OR restricted IS NULL THEN
    RAISE EXCEPTION 'pilot repositories missing from the seed';
  END IF;

  INSERT INTO pull_request_projections (repository_id, number, title, state, updated_at)
  VALUES (repo, 8801, 'An open source change', 'open', now()),
         (restricted, 8802, 'A client-confidential change', 'open', now());

  SELECT count(*) INTO n FROM pull_request_projections WHERE number IN (8801, 8802);
  IF n <> 2 THEN RAISE EXCEPTION 'setup failed'; END IF;
END
$$;

-- As the application role with NO identity: the situation of any unauthenticated
-- or system query. It must see no projections at all.
SET LOCAL ROLE workgraph_app;

DO $$
DECLARE n integer;
BEGIN
  SELECT count(*) INTO n FROM pull_request_projections;
  IF n <> 0 THEN
    RAISE EXCEPTION 'row-level security is not protecting projections: % row(s) readable with no identity', n;
  END IF;

  -- github_deliveries carries raw payloads and has no read policy at all.
  SELECT count(*) INTO n FROM github_deliveries;
  IF n <> 0 THEN
    RAISE EXCEPTION '% delivery payload(s) readable with no identity', n;
  END IF;

  -- The system path must still work, through the functions, with no identity.
  SELECT count(*) INTO n FROM system_github_repositories();
  IF n = 0 THEN
    RAISE EXCEPTION 'the system path sees no repositories; reconciliation would no-op silently';
  END IF;

  -- Recording and marking a delivery: the webhook path, which also has no user.
  IF system_record_delivery('recon-test-1', 'pull_request', '{}'::jsonb) IS NOT TRUE THEN
    RAISE EXCEPTION 'the system path could not record a delivery';
  END IF;
  -- The same delivery again is a retry, not a failure.
  IF system_record_delivery('recon-test-1', 'pull_request', '{}'::jsonb) IS NOT FALSE THEN
    RAISE EXCEPTION 'a duplicate delivery was recorded twice';
  END IF;

  SELECT count(*) INTO n FROM system_pending_deliveries(100) WHERE delivery_id = 'recon-test-1';
  IF n <> 1 THEN RAISE EXCEPTION 'the delivery is not pending after being recorded'; END IF;

  PERFORM system_mark_delivery_processed('recon-test-1');
  SELECT count(*) INTO n FROM system_pending_deliveries(100) WHERE delivery_id = 'recon-test-1';
  IF n <> 0 THEN RAISE EXCEPTION 'a processed delivery is still pending'; END IF;

  RAISE NOTICE 'system path assertions passed with no identity';
END
$$;

RESET ROLE;

-- With a real identity, a member sees their project's projections and not the
-- restricted client's. This is the property the leak broke.
DO $$
DECLARE u uuid; n integer;
BEGIN
  -- A member of an internal project who is NOT on the restricted client work,
  -- and who holds no organisation-wide role.
  --
  -- The role exclusion matters: can_read_project also grants organisation_admin
  -- and executive sight of every project, so picking such a user would make
  -- this assertion fail for a legitimate reason and look like a leak.
  SELECT m.user_id INTO u
    FROM project_memberships m
    JOIN projects p ON p.id = m.project_id
   WHERE p.slug <> 'nged'
     AND m.user_id NOT IN (
       SELECT m2.user_id FROM project_memberships m2
         JOIN projects p2 ON p2.id = m2.project_id
        WHERE p2.slug = 'nged')
     AND m.user_id NOT IN (
       SELECT g.user_id FROM role_grants g
        WHERE g.project_id IS NULL
          AND g.role_name IN ('organisation_admin', 'executive')
          AND (g.expires_at IS NULL OR g.expires_at > now()))
   LIMIT 1;

  IF u IS NULL THEN
    RAISE NOTICE 'no non-restricted member in the seed; skipping the membership assertion';
    RETURN;
  END IF;

  PERFORM set_config('workgraph.user_id', u::text, true);
  SET LOCAL ROLE workgraph_app;

  SELECT count(*) INTO n FROM pull_request_projections WHERE number = 8802;
  IF n <> 0 THEN
    RAISE EXCEPTION 'a non-member read the restricted client projection';
  END IF;

  RESET ROLE;
  RAISE NOTICE 'membership assertions passed';
END
$$;

ROLLBACK;
