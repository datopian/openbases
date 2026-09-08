-- A delivery for a repository no project has attached is parked, not retried
-- forever.
--
-- The App is installed on every repository in the organisation, deliberately:
-- somebody creating a project must be able to attach a repository without an
-- admin widening an installation first. The consequence is that webhooks arrive
-- for every repository Datopian has, and Workgraph has a project for a dozen of
-- them.
--
-- internal/reconcile left those deliveries pending on purpose -- "registering
-- the repository later must be able to recover its history, and that is only
-- possible while the receipt is still pending". That trade was right when the
-- installation covered a handful of repositories. With it covering all of them
-- the same line produced, on staging: 409 unprocessed deliveries from
-- dx-helm-neso, ssen-portal, DV_Solutions and others with no project, the
-- oldest six days old, re-examined every five seconds forever, and a
-- platform_webhook_backlog alert that could never clear.
--
-- The same file already reasons correctly about the sibling case, for event
-- types it does not project: "Marking it processed keeps it out of every future
-- pass; leaving it would make the backlog grow without bound and hide the
-- receipts that genuinely need attention." That is exactly what happened here.
--
-- Parked rather than discarded, so the original intent survives: a parked
-- delivery is still unprocessed, is retried hourly instead of every pass, and
-- is replayed in full if its repository is attached later. What changes is that
-- it stops being counted as a backlog, because nothing is stuck -- there is
-- simply no project that wants it.
BEGIN;

ALTER TABLE github_deliveries ADD COLUMN IF NOT EXISTS parked_at timestamptz;

COMMENT ON COLUMN github_deliveries.parked_at IS
    'When this delivery was last found to be for a repository no project has '
    'attached. Still unprocessed and still replayable: retried hourly, and '
    'excluded from the backlog alert because nothing is stuck.';

-- The retry query orders unparked first, so this index is what keeps that
-- cheap once there are thousands of parked rows.
CREATE INDEX IF NOT EXISTS github_deliveries_pending
    ON github_deliveries (parked_at NULLS FIRST, received_at)
    WHERE processed_at IS NULL;

-- Parked rows go LAST, and that ordering is load-bearing.
--
-- The previous version ordered by received_at alone. Parked deliveries are the
-- oldest rows in the table, so with 409 of them they would fill every batch and
-- a delivery that arrived a second ago would wait behind six-day-old noise for
-- somebody else's repository. Fresh work first, parked work with whatever
-- capacity is left.
--
-- Retried after an hour rather than never, because that is what makes
-- attaching a repository recover its history: the parked deliveries for it
-- replay on the next hourly sweep.
CREATE OR REPLACE FUNCTION system_pending_deliveries(p_limit integer)
RETURNS TABLE (delivery_id text, event_type text, payload jsonb)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT d.delivery_id, d.event_type, d.payload
      FROM github_deliveries d
     WHERE d.processed_at IS NULL
       AND (d.parked_at IS NULL OR d.parked_at < now() - interval '1 hour')
     ORDER BY (d.parked_at IS NOT NULL), d.received_at
     LIMIT p_limit;
$$;

DROP FUNCTION IF EXISTS system_park_delivery(text);

CREATE OR REPLACE FUNCTION system_park_delivery(p_delivery_id text)
RETURNS void
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    UPDATE github_deliveries
       SET parked_at = now()
     WHERE delivery_id = p_delivery_id
       AND processed_at IS NULL;
$$;

DROP FUNCTION IF EXISTS system_discard_parked_deliveries(integer);

-- Parking bounds the churn; this bounds the growth.
--
-- A delivery parked for weeks describes a repository nobody attached, and its
-- content is stale anyway: attaching a repository resyncs its pull requests
-- from the API, so replay reconstructs intermediate states rather than current
-- ones. Discarded by marking processed, which is what the same file already
-- does for an event type it cannot project.
CREATE OR REPLACE FUNCTION system_discard_parked_deliveries(p_older_than_days integer)
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE n integer;
BEGIN
    IF p_older_than_days IS NULL OR p_older_than_days < 1 THEN
        -- Zero would discard a delivery parked moments ago, which is the one
        -- case the hourly retry exists to serve.
        RAISE EXCEPTION 'a retention window of % days would discard deliveries that are still replayable', p_older_than_days;
    END IF;
    WITH gone AS (
        UPDATE github_deliveries
           SET processed_at = now()
         WHERE processed_at IS NULL
           AND parked_at IS NOT NULL
           AND received_at < now() - make_interval(days => p_older_than_days)
        RETURNING 1
    )
    SELECT count(*) INTO n FROM gone;
    RETURN n;
END
$$;

-- The alert counts what is STUCK, not what is parked.
--
-- A parked delivery is not a backlog: no project wants it, nothing is failing,
-- and no action clears it. Counting it made the alert permanent, and a check
-- nobody can clear is a check everybody learns to ignore -- which also hides
-- the one delivery that really is stuck behind four hundred that are not.
CREATE OR REPLACE FUNCTION system_webhook_backlog()
RETURNS TABLE (unprocessed bigint, oldest_seconds numeric)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT count(*),
           COALESCE(EXTRACT(EPOCH FROM (now() - min(received_at))), 0)::numeric
      FROM github_deliveries
     WHERE processed_at IS NULL
       AND parked_at IS NULL;
$$;

REVOKE ALL ON FUNCTION system_park_delivery(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_discard_parked_deliveries(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_park_delivery(text) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_discard_parked_deliveries(integer) TO workgraph_app;

COMMIT;
