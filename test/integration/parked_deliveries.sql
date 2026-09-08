-- A delivery for a repository no project attached is parked: still replayable,
-- no longer a backlog, and never ahead of fresh work.
--
-- The App is installed on every repository in the organisation on purpose, so
-- webhooks arrive for repositories Workgraph has no project for. Those used to
-- stay pending forever -- 409 of them on staging, oldest six days, re-examined
-- every five seconds, with an alert that could not clear.
--
-- Runs as workgraph_app with no app user, which is what the reconciler is:
-- github_deliveries is protected and every access below goes through a
-- SECURITY DEFINER function. As the owner, RLS is bypassed and the ordering and
-- exclusion below would be tested through the wrong lens.
\set ON_ERROR_STOP on

BEGIN;

DO $$
DECLARE
  n_before bigint; n_after bigint; oldest numeric;
  first_id text; ids text[]; discarded integer;
BEGIN
  -- Three deliveries: one fresh and wanted, one parked long ago, one parked
  -- just now. Received_at ordered so the parked ones are the OLDEST, which is
  -- the situation that starved fresh work.
  INSERT INTO github_deliveries (delivery_id, event_type, payload, received_at, parked_at)
  VALUES ('pk-old-parked',  'pull_request', '{"probe": true}'::jsonb, now() - interval '6 days',  now() - interval '3 hours'),
         ('pk-just-parked', 'pull_request', '{"probe": true}'::jsonb, now() - interval '5 days',  now() - interval '1 minute'),
         ('pk-fresh',       'pull_request', '{"probe": true}'::jsonb, now() - interval '1 minute', NULL);

  SET LOCAL ROLE workgraph_app;
  IF current_user <> 'workgraph_app' THEN
      RAISE EXCEPTION 'expected to be workgraph_app, am %', current_user;
  END IF;

  -- 1. The alert counts what is stuck. A parked delivery is not stuck: no
  --    project wants it, nothing is failing, and no action clears it.
  SELECT unprocessed, oldest_seconds INTO n_after, oldest FROM system_webhook_backlog();
  IF n_after <> 1 THEN
      RAISE EXCEPTION 'the backlog counts % deliveries; only the unparked one is stuck', n_after;
  END IF;
  IF oldest > 600 THEN
      RAISE EXCEPTION 'the backlog age is %s, so it is still measuring a parked delivery', oldest;
  END IF;

  -- 2. Fresh work comes FIRST, ahead of older parked work. Without this the
  --    parked rows are the oldest in the table, fill every batch, and a
  --    delivery that arrived a second ago waits behind six-day-old noise for
  --    somebody else's repository.
  SELECT array_agg(delivery_id ORDER BY ord) INTO ids
    FROM (SELECT delivery_id, row_number() OVER () AS ord
            FROM system_pending_deliveries(10)) q;
  IF ids[1] <> 'pk-fresh' THEN
      RAISE EXCEPTION 'the first pending delivery is %, not the fresh one; parked work is starving it', ids[1];
  END IF;

  -- 3. A delivery parked within the hour is not retried. This is the churn
  --    that made the worker re-examine 200 rows every five seconds.
  IF 'pk-just-parked' = ANY(ids) THEN
      RAISE EXCEPTION 'a delivery parked a minute ago is being retried immediately';
  END IF;

  -- 4. A delivery parked over an hour ago IS retried, because that is what
  --    makes attaching a repository later recover its history.
  IF NOT ('pk-old-parked' = ANY(ids)) THEN
      RAISE EXCEPTION 'a delivery parked three hours ago is never retried, so attaching '
          'its repository would not recover its history';
  END IF;

  -- 5. Retention bounds the growth parking allows. pk-old-parked is six days
  --    old, so a 5-day window discards it and a 30-day one does not.
  SELECT system_discard_parked_deliveries(30) INTO discarded;
  IF discarded <> 0 THEN
      RAISE EXCEPTION 'a 30-day window discarded % deliveries, the oldest being 6 days', discarded;
  END IF;
  SELECT system_discard_parked_deliveries(5) INTO discarded;
  IF discarded <> 1 THEN
      RAISE EXCEPTION 'a 5-day window discarded %, not the single 6-day-old parked delivery', discarded;
  END IF;

  -- 6. And the fresh one is untouched by retention, parked or not.
  IF EXISTS (SELECT 1 FROM github_deliveries WHERE delivery_id = 'pk-fresh' AND processed_at IS NOT NULL) THEN
      RAISE EXCEPTION 'retention discarded an unparked delivery';
  END IF;
END
$$;

-- A window that would discard something still replayable is refused, rather
-- than quietly deleting the case the hourly retry exists to serve.
DO $$
DECLARE n integer;
BEGIN
  SELECT system_discard_parked_deliveries(0) INTO n;
  RAISE EXCEPTION 'a zero-day retention window was accepted, discarding %', n;
EXCEPTION
  WHEN raise_exception THEN
    IF position('still replayable' IN SQLERRM) = 0 THEN
        RAISE EXCEPTION 'refused for the wrong reason: %', SQLERRM;
    END IF;
END
$$;

ROLLBACK;

SELECT 'parked deliveries leave the backlog, wait behind fresh work, still replay, and expire' AS result;
