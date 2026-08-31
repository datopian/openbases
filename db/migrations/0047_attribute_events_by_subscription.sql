-- wg:backfill — this migration attributes rows that already arrived, as well as
-- changing the function that attributes future ones.
--
-- Attribute a delivery to its source through the subscription that produced it.
--
-- system_record_event resolved the source by matching the delivery's target
-- against event_sources.external_id. The first real delivery showed that cannot
-- work. Google's attributes are:
--
--   ce-source   workspaceevents.googleapis.com/subscriptions/drive-file-1E7uX…
--   ce-subject  googleapis.com/drive/v3/files/1c70_FeU54lxUtUWeTDU6BDiARl8AoESU
--
-- Neither names the drive. ce-subject names the FILE, and the drive id appears
-- nowhere — so every allow-listed event was recorded with source_id NULL, which
-- is the same result an event from an unknown source gets. That makes "a
-- non-allow-listed source is ignored" unfalsifiable: nothing resolves, so the
-- test passes whether the allow-list works or not.
--
-- ce-source names the subscription, and event_subscriptions.google_name is
-- exactly that. So the subscription is the join key, and it is a better one than
-- the drive id: it is what Google sends on every delivery, and a subscription
-- belongs to precisely one source by construction.
BEGIN;

-- Kept for evidence, not only for the join. "Which subscription produced this"
-- is the first question asked when deliveries stop or arrive twice, and the raw
-- payload is a poor place to answer it from.
ALTER TABLE event_receipts ADD COLUMN IF NOT EXISTS subscription text;

CREATE INDEX IF NOT EXISTS event_receipts_subscription
    ON event_receipts (subscription, received_at DESC);

-- Dropped rather than overloaded. A five-argument version with a default and a
-- four-argument version are ambiguous for a four-argument call, and PostgreSQL
-- reports that at call time rather than at migration time.
DROP FUNCTION IF EXISTS system_record_event(text, text, text, jsonb);

CREATE OR REPLACE FUNCTION system_record_event(
    p_message_id   text,
    p_event_type   text,
    p_target       text,
    p_payload      jsonb,
    p_subscription text DEFAULT NULL
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

    -- First by subscription, which is authoritative: a subscription belongs to
    -- exactly one source. Suffix match because Google prefixes the resource
    -- name with the service host, and we store it as Google's API returns it.
    IF p_subscription IS NOT NULL AND length(trim(p_subscription)) > 0 THEN
        SELECT sub.source_id INTO v_source
          FROM event_subscriptions sub
         WHERE sub.google_name IS NOT NULL
           AND (p_subscription = sub.google_name
                OR p_subscription LIKE '%' || sub.google_name)
         LIMIT 1;
    END IF;

    -- Then by target, which still resolves a Meet delivery naming its space and
    -- anything whose subscription we have since forgotten. Not a replacement
    -- for the above: it cannot resolve a Drive file to its drive.
    IF v_source IS NULL AND p_target IS NOT NULL THEN
        SELECT id INTO v_source
          FROM event_sources
         WHERE p_target LIKE '%' || external_id
         LIMIT 1;
    END IF;

    INSERT INTO event_receipts
        (message_id, source_id, event_type, target, subscription, payload)
    VALUES
        (p_message_id, v_source, p_event_type, p_target, p_subscription, p_payload)
    ON CONFLICT (message_id) DO NOTHING;

    -- FOUND is false when the conflict fired, which is exactly "we already had
    -- this one".
    RETURN FOUND;
END
$$;

REVOKE ALL ON FUNCTION system_record_event(text, text, text, jsonb, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_record_event(text, text, text, jsonb, text) TO workgraph_app;

-- Backfill what already arrived, from the attributes kept in the payload.
--
-- One row on staging at the time of writing: the file.v3.trashed that proved
-- the delivery path. Attributing it retrospectively is the difference between
-- evidence that the path works and evidence that the path works AND the event
-- is usable.
UPDATE event_receipts r
   SET subscription = coalesce(r.subscription,
                               r.payload -> 'message' -> 'attributes' ->> 'ce-source')
 WHERE r.subscription IS NULL;

UPDATE event_receipts r
   SET source_id = sub.source_id
  FROM event_subscriptions sub
 WHERE r.source_id IS NULL
   AND sub.google_name IS NOT NULL
   AND r.subscription IS NOT NULL
   AND (r.subscription = sub.google_name OR r.subscription LIKE '%' || sub.google_name);

COMMIT;
