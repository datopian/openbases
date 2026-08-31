-- The system path for the subscription reconciler (WP-H1, wg-8yv.20).
--
-- SECURITY DEFINER for the reason every other system function here is: the
-- reconciler runs on a timer with no user, and event_sources,
-- event_subscriptions and event_receipts are all behind FORCE ROW LEVEL
-- SECURITY. Without this it would read an empty allow-list, conclude there is
-- nothing to subscribe to, and delete every subscription it found — the
-- reconciliation equivalent of the "reads empty while writes succeed" trap that
-- has bitten this schema five times.
BEGIN;

-- jsonb -> text[], refusing anything that is not an array of strings.
--
-- Its own function so the refusal happens once. A JSON object silently
-- converted to an array of its values would produce a subscription asking for
-- event types nobody chose, and Google would reject the whole create.
CREATE OR REPLACE FUNCTION jsonb_to_text_array(p jsonb)
RETURNS text[]
LANGUAGE plpgsql
IMMUTABLE
AS $$
BEGIN
    IF p IS NULL OR p = 'null'::jsonb THEN
        RETURN NULL;
    END IF;
    IF jsonb_typeof(p) <> 'array' THEN
        RAISE EXCEPTION 'expected a JSON array of event types, got %', jsonb_typeof(p);
    END IF;
    RETURN ARRAY(SELECT jsonb_array_elements_text(p));
END
$$;

-- The allow-list and what we believe about each source's subscription, in one
-- read. Reconcile compares them, so fetching them separately invites a race
-- where a source is added between the two queries and looks like a
-- subscription with no source.
CREATE OR REPLACE FUNCTION system_event_sources()
RETURNS TABLE (
    source_id       uuid,
    kind            text,
    external_id     text,
    display_name    text,
    visibility      text,
    enabled         boolean,
    google_name     text,
    state           text,
    expires_at      timestamptz,
    -- jsonb, not text[]. The array crosses a database/sql boundary, and array
    -- codecs differ between drivers in ways that fail at run time rather than
    -- compile time; JSON has one representation everywhere. It cost a whole
    -- driver dependency to find that out.
    event_types     jsonb
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT s.id, s.kind, s.external_id, s.display_name, s.visibility, s.enabled,
           sub.google_name, coalesce(sub.state, 'pending'), sub.expires_at,
           to_jsonb(coalesce(sub.event_types, '{}'::text[]))
      FROM event_sources s
      LEFT JOIN event_subscriptions sub ON sub.source_id = s.id
     ORDER BY s.kind, s.display_name;
$$;

-- There is deliberately no function for "subscriptions whose source is gone".
--
-- event_subscriptions.source_id is ON DELETE CASCADE, so removing a source
-- removes its subscription row with it: a database orphan cannot exist. The
-- orphan that CAN exist is a subscription live in Google that we have no record
-- of — one a crashed pass created and never wrote down, or one left by an
-- earlier deployment. It delivers into our topic and nothing renews or deletes
-- it, so it is found by listing subscriptions from Google and comparing, not by
-- querying here. See Reconciler.adopt.

-- Record what Google now holds for a source.
--
-- One function for create, renew and reactivate because all three end in the
-- same place: this is what the subscription is now. Separate functions would
-- differ only in which fields they left alone, and a field left alone by
-- accident is how a stale expiry survives a renewal.
CREATE OR REPLACE FUNCTION system_record_subscription(
    p_source_id   uuid,
    p_google_name text,
    p_state       text,
    p_expires_at  timestamptz,
    p_event_types jsonb,
    p_error       text DEFAULT NULL
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    IF p_source_id IS NULL THEN
        RAISE EXCEPTION 'a subscription needs a source';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM event_sources WHERE id = p_source_id) THEN
        -- Refused rather than inserted. A subscription for a source that is not
        -- allow-listed is the thing the allow-list exists to prevent, and
        -- recording one would make the next reconciliation delete it — a loop
        -- that creates and destroys forever while looking busy.
        RAISE EXCEPTION 'source % is not allow-listed', p_source_id;
    END IF;

    INSERT INTO event_subscriptions
        (source_id, google_name, state, expires_at, event_types, last_error, last_renewed_at)
    VALUES
        (p_source_id, p_google_name, p_state, p_expires_at,
         -- An empty array rather than NULL for "we do not know": the column is
         -- NOT NULL, and a NULL here would abort the write that records a
         -- failure — losing exactly the information a failure exists to leave.
         coalesce(jsonb_to_text_array(p_event_types), '{}'),
         p_error, CASE WHEN p_error IS NULL THEN now() END)
    ON CONFLICT (source_id) DO UPDATE SET
        google_name     = excluded.google_name,
        state           = excluded.state,
        expires_at      = excluded.expires_at,
        event_types     = excluded.event_types,
        last_error      = excluded.last_error,
        -- Only advanced on success. A failed attempt must not look like a
        -- renewal, or "when did this last work" stops being answerable.
        last_renewed_at = CASE WHEN p_error IS NULL THEN now()
                               ELSE event_subscriptions.last_renewed_at END,
        updated_at      = now();
    RETURN true;
END
$$;

-- Mark a subscription gone. Kept as a row rather than deleted so that the
-- history of what we once subscribed to survives, and so a delete that failed
-- halfway is visible rather than absent.
CREATE OR REPLACE FUNCTION system_forget_subscription(p_source_id uuid)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE n integer;
BEGIN
    UPDATE event_subscriptions
       SET state = 'deleted', google_name = NULL, expires_at = NULL, updated_at = now()
     WHERE source_id = p_source_id;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n > 0;
END
$$;

-- What arrived, for the acceptance criterion about discovery and for answering
-- "did anything come in this week".
CREATE OR REPLACE FUNCTION system_event_summary(p_since timestamptz DEFAULT NULL)
RETURNS TABLE (
    display_name text,
    kind         text,
    event_type   text,
    deliveries   bigint,
    latest       timestamptz,
    unprocessed  bigint
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT coalesce(s.display_name, '(unrecognised source)'),
           coalesce(s.kind, '-'),
           coalesce(r.event_type, '(none)'),
           count(*),
           max(r.received_at),
           count(*) FILTER (WHERE r.processed_at IS NULL)
      FROM event_receipts r
      LEFT JOIN event_sources s ON s.id = r.source_id
     WHERE p_since IS NULL OR r.received_at >= p_since
     GROUP BY 1, 2, 3
     ORDER BY 5 DESC;
$$;

REVOKE ALL ON FUNCTION system_event_sources() FROM PUBLIC;
REVOKE ALL ON FUNCTION system_record_subscription(uuid, text, text, timestamptz, jsonb, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_forget_subscription(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_event_summary(timestamptz) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION system_event_sources() TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_record_subscription(uuid, text, text, timestamptz, jsonb, text) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_forget_subscription(uuid) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_event_summary(timestamptz) TO workgraph_app;

COMMIT;
