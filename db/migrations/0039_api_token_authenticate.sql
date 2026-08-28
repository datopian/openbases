-- Authenticating a personal API token, which has to happen before there is a
-- user to authenticate as (wg-p4h.2).
--
-- Every other read in this schema runs under authz.WithUser, which sets
-- workgraph.user_id and lets the policies decide. This one cannot: the token IS
-- how the user is established, so at lookup time current_app_user() is NULL and
-- api_tokens' own policy would deny. That is the correct behaviour for the
-- policy and the reason this function exists.
--
-- SECURITY DEFINER, like system_work_overview and the other system_ reads, with
-- search_path pinned so the definer's rights cannot be redirected at a table an
-- attacker controls.
--
-- The narrowness is the safety argument. It takes a digest and nothing else. It
-- cannot be used to enumerate: with no matching live token it returns no rows,
-- and the only way to obtain a row is to already hold the token whose SHA-256
-- you are presenting. It returns the user id and the scopes and nothing that
-- would help forge a second credential.

BEGIN;

CREATE OR REPLACE FUNCTION system_api_token_authenticate(p_digest bytea)
RETURNS TABLE (
    token_id uuid,
    user_id  uuid,
    scopes   text[]
)
LANGUAGE sql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
    SELECT t.id, t.user_id, t.scopes
      FROM api_tokens t
      JOIN users u ON u.id = t.user_id
     WHERE t.token_sha256 = p_digest
       -- Revocation takes effect on the next request. No cache, no TTL: a
       -- leaked token is revoked at the moment somebody clicks revoke, not up
       -- to some window later.
       AND t.revoked_at IS NULL
       AND t.expires_at > now()
       -- A suspended or removed user's tokens stop working with them. Without
       -- this, offboarding would have to remember the tokens separately, and
       -- the one it forgot would be the one that mattered.
       AND u.status = 'active';
$$;

-- Recording use is a SEPARATE call, deliberately.
--
-- Folding it into the lookup would make authentication a write, so every
-- authenticated request would take a row lock on the token and a read-only
-- workload could not run against a replica. It would also mean a failure to
-- record use becomes a failure to authenticate, which is the wrong direction:
-- losing a timestamp is a nuisance, refusing a valid request is an outage.
--
-- The caller decides when to bother, and coarsens it. Updating on every request
-- is a write per request for a field nobody reads to the minute; the guard here
-- means a token used continuously is written to once per interval instead.
CREATE OR REPLACE FUNCTION system_api_token_record_use(
    p_token_id uuid,
    p_coarsen  interval DEFAULT interval '5 minutes'
) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
    UPDATE api_tokens
       SET last_used_at = now()
     WHERE id = p_token_id
       AND (last_used_at IS NULL OR last_used_at < now() - p_coarsen);
$$;

REVOKE ALL ON FUNCTION system_api_token_authenticate(bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_api_token_record_use(uuid, interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_api_token_authenticate(bytea) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_api_token_record_use(uuid, interval) TO workgraph_app;

COMMIT;
