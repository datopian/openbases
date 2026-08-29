-- Idempotency keys for write endpoints (wg-p4h.4).
--
-- Plan §12.3 requires them and no write endpoint has one. Until now the only
-- writer was a person clicking once. An agent retries on a timeout it cannot
-- distinguish from a failure, and "dispatch this bead" executed twice spends
-- twice.
--
-- The pattern is already in this repository twice, inbound: internal/githubapp
-- keys on X-GitHub-Delivery, and internal/cost drops records with no gateway
-- call id because "without the gateway's id there is no idempotency key". Same
-- discipline, other direction.

BEGIN;

CREATE TABLE idempotency_keys (
    -- The caller's key, scoped to the user. Two people may use the same key
    -- without colliding; the same person may not, which is the point.
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key          text NOT NULL CHECK (length(key) BETWEEN 1 AND 255),

    -- The route this key was used on. A key replayed against a DIFFERENT route
    -- is a client bug, and treating it as a hit would return one endpoint's
    -- response to another endpoint's call.
    route        text NOT NULL,

    -- A digest of the request body. A repeat with the same key and a different
    -- body is a conflict rather than a replay: the caller believes they are
    -- sending something new, and silently returning the old response would give
    -- them the wrong answer and hide the mistake.
    request_sha  bytea NOT NULL CHECK (length(request_sha) = 32),

    -- What to replay. Stored rather than recomputed, because the point is that
    -- the effect happens once.
    status_code  integer NOT NULL CHECK (status_code BETWEEN 100 AND 599),
    response     jsonb   NOT NULL,

    created_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, key)
);

-- Keys are worth remembering for as long as a client might retry, not forever.
-- The sweep is a query rather than a policy engine: anything older than a day
-- cannot still be an in-flight retry.
CREATE INDEX idempotency_keys_age ON idempotency_keys (created_at);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;

-- A caller sees only their own keys. A stored response can contain anything the
-- endpoint returned, so this table inherits the sensitivity of every write
-- endpoint at once and is scoped accordingly.
CREATE POLICY idempotency_keys_read ON idempotency_keys
    FOR SELECT USING (user_id = current_app_user());

CREATE POLICY idempotency_keys_insert ON idempotency_keys
    FOR INSERT WITH CHECK (user_id = current_app_user());

-- Deliberately no UPDATE policy. A stored response is a record of what already
-- happened; rewriting it would make a replay return something the first call
-- never returned.
CREATE POLICY idempotency_keys_delete ON idempotency_keys
    FOR DELETE USING (user_id = current_app_user());

-- Discard keys older than the retry horizon.
CREATE OR REPLACE FUNCTION system_sweep_idempotency_keys(p_older_than interval DEFAULT interval '24 hours')
RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
DECLARE n bigint;
BEGIN
    DELETE FROM idempotency_keys WHERE created_at < now() - p_older_than;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END;
$$;

REVOKE ALL ON FUNCTION system_sweep_idempotency_keys(interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_sweep_idempotency_keys(interval) TO workgraph_app;

COMMIT;
