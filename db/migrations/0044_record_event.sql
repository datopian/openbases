-- Recording a Pub/Sub delivery, idempotently (WP-H1, ADR-0026).
--
-- A function rather than an INSERT in the handler for the reason every other
-- write here is one: event_receipts is behind RLS, and the handler runs with no
-- user — a push from Google is authenticated by Google, not by a person. The
-- system path is SECURITY DEFINER, as it is for usage records and the work
-- queue.
--
-- It returns whether the row was NEW. Pub/Sub is at-least-once by design, so a
-- repeat is expected rather than exceptional, and the caller needs to tell
-- "recorded" from "already had it" to avoid doing the work twice.
BEGIN;

CREATE OR REPLACE FUNCTION system_record_event(
    p_message_id text,
    p_event_type text,
    p_target     text,
    p_payload    jsonb
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_source uuid;
BEGIN
    IF p_message_id IS NULL OR length(trim(p_message_id)) = 0 THEN
        -- Without Pub/Sub's message id there is no idempotency key, and a
        -- delivery we cannot deduplicate would be processed again on every
        -- redelivery. The handler refuses these too; this is the second door.
        RAISE EXCEPTION 'a delivery needs a message id';
    END IF;

    -- Resolve the source from the target if we recognise it. NOT a join
    -- condition and not required: an event for a source we do not allow-list is
    -- still worth recording, because "why did nothing happen" is a question the
    -- receipts have to be able to answer.
    IF p_target IS NOT NULL THEN
        SELECT id INTO v_source
          FROM event_sources
         WHERE p_target LIKE '%' || external_id
         LIMIT 1;
    END IF;

    INSERT INTO event_receipts (message_id, source_id, event_type, target, payload)
    VALUES (p_message_id, v_source, p_event_type, p_target, p_payload)
    ON CONFLICT (message_id) DO NOTHING;

    -- FOUND is false when the conflict fired, which is exactly "we already had
    -- this one".
    RETURN FOUND;
END
$$;

REVOKE ALL ON FUNCTION system_record_event(text, text, text, jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_record_event(text, text, text, jsonb) TO workgraph_app;

COMMIT;
