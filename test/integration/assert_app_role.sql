-- Assert this session has actually dropped to workgraph_app.
--
-- Include immediately after `SET LOCAL ROLE workgraph_app`:
--
--   SET LOCAL ROLE workgraph_app;
--   \ir assert_app_role.sql
--
-- \ir resolves relative to the including script, so it works whatever directory
-- psql was invoked from.
--
-- Why this exists. CI connects as `postgres`, a superuser, and a superuser does
-- not merely satisfy row-level security policies — it never evaluates them, and
-- FORCE ROW LEVEL SECURITY does not change that. So a visibility test that runs
-- as postgres sees every row whatever the policies say, and an assertion like
-- "a non-member cannot read this project" passes identically whether the policy
-- is correct, wrong, or absent entirely.
--
-- Every such test in this suite therefore drops to workgraph_app first, and
-- until now that was the whole of the protection: a convention, one deleted or
-- mistyped line away from turning every downstream assertion into a test that
-- cannot fail while still reporting success. Two ways it happens for real —
-- neither hypothetical, both silent:
--
--   the SET is removed or renamed during a refactor, and the suite stays green;
--
--   `SET LOCAL` is used outside a transaction block, where PostgreSQL emits a
--   WARNING and applies nothing. Every test here opens an explicit BEGIN, so
--   they are correct today, but a new test written without one would warn into
--   the CI log and pass.
--
-- This is the same guard test/load/leak_test.sql carries, and for the same
-- reason: that test once compared RLS against RLS and reported 41 leaks that
-- did not exist. A permission test that cannot fail is worse than no test,
-- because it is evidence for a guarantee nobody has checked.
DO $$
BEGIN
  IF current_user <> 'workgraph_app' THEN
    RAISE EXCEPTION
      'running as %, not workgraph_app: row-level security is bypassed and the '
      'assertions that follow would pass regardless of the policies. Add '
      '`SET LOCAL ROLE workgraph_app` inside an explicit transaction.',
      current_user;
  END IF;
END $$;
