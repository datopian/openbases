-- Registering an ingested source, and what it looked like when we read it
-- (WP-H2, plan section 14.3).
--
-- SECURITY DEFINER for the reason every system path here is: ingestion runs on
-- a timer with no user, and knowledge_sources is behind row-level security. A
-- direct insert would be refused, and a direct read would return nothing.
BEGIN;

-- Register a source, idempotently on (provider, provider_source_id, revision).
--
-- The revision is part of the key on purpose. A transcript that Google
-- regenerates is a NEW revision of the same source, not an update to the old
-- one: the old row and its evidence stay, because an extraction that cited the
-- earlier text must remain explicable after the text changes.
CREATE OR REPLACE FUNCTION system_register_source(
    p_provider           text,
    p_provider_source_id text,
    p_provider_revision  text,
    p_source_type        text,
    p_project_id         uuid,
    p_visibility         text,
    p_captured_at        timestamptz,
    p_retention_class    text DEFAULT 'standard',
    p_owner_email        text DEFAULT NULL,
    p_injection_suspected boolean DEFAULT false
) RETURNS uuid
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_org   uuid;
    v_owner uuid;
    v_id    uuid;
BEGIN
    SELECT id INTO v_org FROM organisations ORDER BY created_at LIMIT 1;
    IF v_org IS NULL THEN
        RAISE EXCEPTION 'no organisation to attach the source to';
    END IF;

    IF p_owner_email IS NOT NULL THEN
        SELECT id INTO v_owner FROM users WHERE lower(primary_email) = lower(p_owner_email);
    END IF;

    -- The classification is passed in rather than defaulted, and there is no
    -- fallback. ADR-0013 makes every derived artefact inherit the strictest
    -- classification of its sources, so a source registered at the wrong level
    -- mis-classifies everything downstream of it -- and 'internal' is the
    -- level that leaks. A caller that does not know must not get a guess.
    IF p_visibility IS NULL THEN
        RAISE EXCEPTION 'a source needs a visibility; there is no safe default';
    END IF;

    INSERT INTO knowledge_sources (
        organisation_id, provider, provider_source_id, provider_revision,
        source_type, owner_user_id, project_id, captured_at, visibility,
        retention_class, prompt_injection_suspected
    ) VALUES (
        v_org, p_provider, p_provider_source_id, p_provider_revision,
        p_source_type, v_owner, p_project_id, p_captured_at, p_visibility,
        coalesce(p_retention_class, 'standard'), coalesce(p_injection_suspected, false)
    )
    ON CONFLICT (provider, provider_source_id, provider_revision) DO UPDATE
        SET visibility  = excluded.visibility,
            project_id  = excluded.project_id,
            captured_at = excluded.captured_at,
            prompt_injection_suspected = excluded.prompt_injection_suspected
    RETURNING id INTO v_id;

    RETURN v_id;
END
$$;

-- Who could see the source when we read it.
--
-- Replaced wholesale per source rather than merged, because an ACL is a
-- snapshot of a moment. Merging would leave a principal who has since been
-- removed sitting in the record as though they still had access, which is the
-- opposite of what an access snapshot is for.
CREATE OR REPLACE FUNCTION system_record_source_acl(
    p_source_id uuid,
    p_entries   jsonb
) RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE n integer;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM knowledge_sources WHERE id = p_source_id) THEN
        RAISE EXCEPTION 'no such source %', p_source_id;
    END IF;
    IF jsonb_typeof(p_entries) <> 'array' THEN
        RAISE EXCEPTION 'expected a JSON array of ACL entries, got %', jsonb_typeof(p_entries);
    END IF;

    DELETE FROM source_acl_entries WHERE source_id = p_source_id;

    INSERT INTO source_acl_entries (source_id, principal, principal_type, role)
    SELECT p_source_id,
           e ->> 'principal',
           e ->> 'principal_type',
           e ->> 'role'
      FROM jsonb_array_elements(p_entries) AS e;

    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END
$$;

-- Mark a delivery handled, so the next pass does not fetch it again.
--
-- Takes the error too. A receipt that failed is marked processed WITH the
-- reason rather than left pending, because a permanent failure left pending is
-- retried forever and hides the receipts that could still succeed behind it.
CREATE OR REPLACE FUNCTION system_mark_receipt_processed(
    p_message_id text,
    p_error      text DEFAULT NULL
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE n integer;
BEGIN
    UPDATE event_receipts
       SET processed_at = now(), error = p_error
     WHERE message_id = p_message_id;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n > 0;
END
$$;

-- The deliveries waiting to be ingested, with what the reader needs to act.
CREATE OR REPLACE FUNCTION system_pending_receipts(p_limit integer DEFAULT 50)
RETURNS TABLE (
    message_id  text,
    event_type  text,
    target      text,
    source_kind text,
    external_id text,
    project_id  uuid,
    visibility  text,
    received_at timestamptz
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT r.message_id, r.event_type, r.target,
           s.kind, s.external_id, s.project_id, s.visibility, r.received_at
      FROM event_receipts r
      JOIN event_sources s ON s.id = r.source_id
     WHERE r.processed_at IS NULL
     ORDER BY r.received_at
     LIMIT greatest(1, least(coalesce(p_limit, 50), 500));
$$;

REVOKE ALL ON FUNCTION system_register_source(text, text, text, text, uuid, text, timestamptz, text, text, boolean) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_record_source_acl(uuid, jsonb) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_mark_receipt_processed(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_pending_receipts(integer) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION system_register_source(text, text, text, text, uuid, text, timestamptz, text, text, boolean) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_record_source_acl(uuid, jsonb) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_mark_receipt_processed(text, text) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_pending_receipts(integer) TO workgraph_app;

COMMIT;
