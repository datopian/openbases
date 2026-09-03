-- Starting, approving and redeeming a device grant (wg-8la).
--
-- Three functions because there are three actors: a client with no credential,
-- a person with a browser, and the same client polling. Each is SECURITY
-- DEFINER because device_authorizations has row-level security and no policy --
-- the functions are the only way in.
BEGIN;

-- The client asks for a code. No credential required, which is the point.
CREATE FUNCTION system_device_start(
    p_device_code_sha256 bytea,
    p_user_code          text,
    p_client_label       text,
    p_scopes             text[],
    p_ttl                interval DEFAULT interval '10 minutes'
) RETURNS timestamptz
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE v_expires timestamptz;
BEGIN
    -- Ten minutes by default and thirty at most. A device code is a window in
    -- which a short human-typed code is guessable; the window is the control.
    IF p_ttl > interval '30 minutes' OR p_ttl <= interval '0' THEN
        RAISE EXCEPTION 'a device grant lives between 0 and 30 minutes';
    END IF;

    -- The scopes are validated HERE rather than at redemption, so a client
    -- asking for something it may never have is refused before a person is
    -- asked to approve it. Approving a grant that cannot be honoured wastes
    -- the one moment somebody was paying attention.
    IF EXISTS (
        SELECT 1 FROM unnest(p_scopes) s
         WHERE s IN ('approval.decide', 'pull_request.merge', 'deployment.execute',
                     'secret.manage', 'policy.manage', 'marketing.publish',
                     'knowledge.classification.downgrade', 'knowledge.review'))
    THEN
        RAISE EXCEPTION 'a token cannot hold that action, so a device grant cannot request it';
    END IF;

    v_expires := now() + p_ttl;

    INSERT INTO device_authorizations
        (device_code_sha256, user_code, client_label, scopes, expires_at)
    VALUES (p_device_code_sha256, upper(trim(p_user_code)), trim(p_client_label),
            coalesce(p_scopes, '{}'), v_expires);

    RETURN v_expires;
END
$$;

-- What the approval page shows before anybody clicks anything.
--
-- Separate from the approval itself so the page can display what is being
-- asked -- which client, which actions -- rather than asking somebody to
-- approve an opaque code. It counts failed lookups on a hit and is the only
-- path that reads by user_code.
CREATE FUNCTION system_device_lookup(p_user_code text)
RETURNS TABLE (client_label text, scopes text[], expires_at timestamptz, already_approved boolean)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE v record;
BEGIN
    SELECT d.* INTO v FROM device_authorizations d
     WHERE d.user_code = upper(trim(p_user_code))
       AND d.expires_at > now()
       AND d.token_id IS NULL;

    IF v.id IS NULL THEN
        -- Deliberately the same answer for "no such code", "expired" and
        -- "already used": a person who mistypes learns nothing about which
        -- codes exist, and neither does anybody guessing.
        RAISE EXCEPTION 'that code is not valid';
    END IF;

    -- Ten wrong guesses against one code and it is dead. The counter is on the
    -- row rather than global so one abandoned code cannot lock out everybody
    -- else's.
    IF v.failed_lookups >= 10 THEN
        RAISE EXCEPTION 'that code is not valid';
    END IF;

    RETURN QUERY SELECT v.client_label, v.scopes, v.expires_at, v.approved_by IS NOT NULL;
END
$$;

-- Counting a wrong guess.
--
-- Its own function because the lookup above raises on a miss, and a raise
-- rolls back -- so the counter cannot be incremented in the same statement
-- that refuses. The caller invokes this when a lookup fails.
CREATE FUNCTION system_device_note_failure(p_user_code text) RETURNS void
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    UPDATE device_authorizations
       SET failed_lookups = failed_lookups + 1
     WHERE user_code = upper(trim(p_user_code));
$$;

-- A person approves. Invoker rights would be wrong -- the caller cannot read
-- the table -- so this is definer, and the approver is passed in from the
-- authenticated session rather than chosen by the caller.
CREATE FUNCTION system_device_approve(p_user_code text, p_user uuid) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE v_id uuid;
BEGIN
    IF p_user IS NULL THEN
        RAISE EXCEPTION 'approving a device grant needs a named person';
    END IF;

    SELECT id INTO v_id FROM device_authorizations
     WHERE user_code = upper(trim(p_user_code))
       AND expires_at > now()
       AND token_id IS NULL
       AND approved_by IS NULL
       AND failed_lookups < 10;

    IF v_id IS NULL THEN
        RETURN false;
    END IF;

    UPDATE device_authorizations
       SET approved_by = p_user, approved_at = now()
     WHERE id = v_id;

    INSERT INTO audit_log (actor_user_id, action, target_type, target_id, outcome, reason)
    VALUES (p_user, 'device.authorization.approve', 'device_authorization',
            v_id::text, 'executed',
            'approved a device grant for ' ||
            (SELECT client_label FROM device_authorizations WHERE id = v_id));

    RETURN true;
END
$$;

-- The client polls with its device code. Returns the state the client needs to
-- act on, in RFC 8628's vocabulary, and the approver's id once there is one so
-- the caller can mint in their name.
CREATE FUNCTION system_device_poll(p_device_code_sha256 bytea)
RETURNS TABLE (state text, approver uuid, scopes text[], client_label text)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE v record;
BEGIN
    SELECT d.* INTO v FROM device_authorizations d
     WHERE d.device_code_sha256 = p_device_code_sha256;

    IF v.id IS NULL THEN
        RETURN QUERY SELECT 'invalid_grant'::text, NULL::uuid, NULL::text[], NULL::text;
        RETURN;
    END IF;
    IF v.token_id IS NOT NULL THEN
        -- Single use. A grant that could be redeemed twice is a grant that
        -- mints a second credential nobody approved.
        RETURN QUERY SELECT 'invalid_grant'::text, NULL::uuid, NULL::text[], NULL::text;
        RETURN;
    END IF;
    IF v.expires_at <= now() THEN
        RETURN QUERY SELECT 'expired_token'::text, NULL::uuid, NULL::text[], NULL::text;
        RETURN;
    END IF;
    IF v.approved_by IS NULL THEN
        RETURN QUERY SELECT 'authorization_pending'::text, NULL::uuid, NULL::text[], NULL::text;
        RETURN;
    END IF;

    RETURN QUERY SELECT 'approved'::text, v.approved_by, v.scopes, v.client_label;
END
$$;

-- Bind the minted token to the grant, which consumes it.
--
-- Separate from the poll because minting happens in Go -- the secret is
-- generated there and never reaches this function -- and because the binding
-- must fail if anything has consumed the grant in between. The WHERE clause is
-- the lock: two concurrent polls cannot both bind.
CREATE FUNCTION system_device_redeem(p_device_code_sha256 bytea, p_token_id uuid)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE v_rows integer;
BEGIN
    UPDATE device_authorizations
       SET token_id = p_token_id, redeemed_at = now()
     WHERE device_code_sha256 = p_device_code_sha256
       AND token_id IS NULL
       AND approved_by IS NOT NULL
       AND expires_at > now();
    GET DIAGNOSTICS v_rows = ROW_COUNT;
    RETURN v_rows = 1;
END
$$;

-- Expired and redeemed grants are removed, not kept.
--
-- The audit_log entry records that somebody approved a grant and for what, so
-- the history survives the row. Keeping the rows themselves would mean keeping
-- a table of user codes, and there is nothing to learn from an expired one.
CREATE FUNCTION system_device_sweep() RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE v_rows integer;
BEGIN
    DELETE FROM device_authorizations
     WHERE expires_at <= now() - interval '1 hour'
        OR redeemed_at <= now() - interval '1 hour';
    GET DIAGNOSTICS v_rows = ROW_COUNT;
    RETURN v_rows;
END
$$;

REVOKE ALL ON FUNCTION system_device_start(bytea, text, text, text[], interval) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_device_lookup(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_device_note_failure(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_device_approve(text, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_device_poll(bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_device_redeem(bytea, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_device_sweep() FROM PUBLIC;

GRANT EXECUTE ON FUNCTION system_device_start(bytea, text, text, text[], interval) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_device_lookup(text) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_device_note_failure(text) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_device_approve(text, uuid) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_device_poll(bytea) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_device_redeem(bytea, uuid) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_device_sweep() TO workgraph_app;

COMMIT;
