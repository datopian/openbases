-- Workspace event subscriptions: the allow-list is the boundary (WP-H1).
--
-- Two properties are asserted here rather than in Go, because both are
-- enforced by the schema and would be untestable from a caller that already has
-- privileges:
--
--   1. The reconciler reads through SECURITY DEFINER functions. If it read the
--      tables directly it would see nothing under RLS, conclude there are no
--      allow-listed sources, and decide to delete every subscription it found.
--   2. A subscription cannot exist for a source that is not allow-listed. That
--      is what "a non-allow-listed source is ignored" means at the point where
--      it can actually be enforced.

BEGIN;

DO $$
DECLARE
  n integer;
  drive_id uuid;
  meet_id uuid;
  ghost uuid := '00000000-0000-0000-0000-0000000000ff';
  got text;
  renewed_before timestamptz;
BEGIN
  -- The four sources the plan names: three shared drives and one recurring
  -- meeting. Seeded, not created by the reconciler, so that adding a source is
  -- a reviewed change to the repository rather than an API call.
  SELECT count(*) INTO n FROM event_sources;
  IF n < 4 THEN
    RAISE EXCEPTION 'expected at least 4 allow-listed sources, found %', n;
  END IF;

  SELECT count(*) INTO n FROM event_sources WHERE visibility = '' OR rationale = '';
  IF n <> 0 THEN
    RAISE EXCEPTION '% source(s) with no visibility or no rationale', n;
  END IF;

  SELECT id INTO drive_id FROM event_sources WHERE kind = 'drive' LIMIT 1;
  SELECT id INTO meet_id FROM event_sources WHERE kind = 'meet' LIMIT 1;
  IF drive_id IS NULL OR meet_id IS NULL THEN
    RAISE EXCEPTION 'both a drive source and a meet source are required; drive=% meet=%',
      drive_id, meet_id;
  END IF;

  -- -----------------------------------------------------------------
  -- The system path returns rows to a caller with no user identity
  -- -----------------------------------------------------------------
  SET LOCAL ROLE workgraph_app;
  -- Proof the SET took effect. CI connects as a superuser, and a superuser never
  -- evaluates row-level security — so without this check, deleting the line
  -- above would turn every assertion below into one that cannot fail.
  IF current_user <> 'workgraph_app' THEN
    RAISE EXCEPTION 'still running as %; the assertions below would bypass RLS', current_user;
  END IF;

  SELECT count(*) INTO n FROM system_event_sources();
  IF n < 4 THEN
    RAISE EXCEPTION 'the reconciler sees only % source(s) through its system path; '
      'seeing none is what makes it delete every subscription', n;
  END IF;

  -- Reading the table directly must return nothing. This is the assertion that
  -- makes the one above meaningful: if the table were readable, the function
  -- would not be load-bearing and someone would eventually remove it.
  SELECT count(*) INTO n FROM event_sources;
  IF n <> 0 THEN
    RAISE EXCEPTION 'workgraph_app read % row(s) from event_sources directly; '
      'RLS is not being enforced', n;
  END IF;

  -- -----------------------------------------------------------------
  -- A subscription for an unknown source is refused
  -- -----------------------------------------------------------------
  BEGIN
    PERFORM system_record_subscription(ghost, 'subscriptions/x', 'active',
                                       now() + interval '2 days', '["a"]'::jsonb, NULL);
    RAISE EXCEPTION 'a subscription was recorded for a source that is not allow-listed';
  EXCEPTION WHEN raise_exception THEN
    IF SQLERRM NOT LIKE '%not allow-listed%' THEN RAISE; END IF;
  END;

  -- Event types arrive as JSON. A JSON object silently converted to an array
  -- of its values would create a subscription asking for types nobody chose,
  -- and Google rejects the whole create — so the conversion refuses instead.
  BEGIN
    PERFORM system_record_subscription(drive_id, 'subscriptions/bad', 'active',
                                       now() + interval '2 days',
                                       '{"not":"an array"}'::jsonb, NULL);
    RAISE EXCEPTION 'a JSON object was accepted as a list of event types';
  EXCEPTION WHEN raise_exception THEN
    IF SQLERRM NOT LIKE '%expected a JSON array%' THEN RAISE; END IF;
  END;

  -- -----------------------------------------------------------------
  -- Recording is idempotent, and a failure does not look like a success
  -- -----------------------------------------------------------------
  PERFORM system_record_subscription(drive_id, 'subscriptions/d1', 'active',
                                     now() + interval '3 days',
                                     '["google.workspace.drive.file.v3.created"]'::jsonb, NULL);
  PERFORM system_record_subscription(drive_id, 'subscriptions/d1', 'active',
                                     now() + interval '3 days',
                                     '["google.workspace.drive.file.v3.created"]'::jsonb, NULL);
  SELECT count(*) INTO n FROM system_event_sources() WHERE google_name = 'subscriptions/d1';
  IF n <> 1 THEN
    RAISE EXCEPTION 'recording the same subscription twice produced % rows', n;
  END IF;

  -- Captured before the failure, because everything here runs in one
  -- transaction and now() is frozen inside it: "later than a second ago" cannot
  -- tell a value the failure wrote from the one the success above wrote.
  RESET ROLE;
  SELECT last_renewed_at INTO renewed_before FROM event_subscriptions WHERE source_id = drive_id;
  SET LOCAL ROLE workgraph_app;

  PERFORM system_record_subscription(drive_id, 'subscriptions/d1', 'failed',
                                     NULL, '["google.workspace.drive.file.v3.created"]'::jsonb,
                                     'the delegation is missing drive.readonly');
  SELECT state INTO got FROM system_event_sources() WHERE source_id = drive_id;
  IF got <> 'failed' THEN
    RAISE EXCEPTION 'a failed attempt recorded state %; a broken subscription that '
      'reads as active is never repaired', got;
  END IF;

  RESET ROLE;

  -- last_renewed_at must not advance on a failure, or "when did this last work"
  -- stops being answerable at exactly the moment it is asked.
  SELECT count(*) INTO n FROM event_subscriptions
   WHERE source_id = drive_id AND last_error IS NULL;
  IF n <> 0 THEN
    RAISE EXCEPTION 'the failure was not recorded against the subscription';
  END IF;
  SELECT count(*) INTO n FROM event_subscriptions
   WHERE source_id = drive_id AND last_renewed_at IS DISTINCT FROM renewed_before;
  IF n <> 0 THEN
    RAISE EXCEPTION 'a failed attempt changed last_renewed_at; "when did this last '
      'actually work" stops being answerable at the moment it is asked';
  END IF;

  -- -----------------------------------------------------------------
  -- Forgetting keeps the row, so the history survives
  -- -----------------------------------------------------------------
  SET LOCAL ROLE workgraph_app;
  PERFORM system_forget_subscription(drive_id);
  SELECT state INTO got FROM system_event_sources() WHERE source_id = drive_id;
  IF got <> 'deleted' THEN
    RAISE EXCEPTION 'after forgetting, state was %', got;
  END IF;
  RESET ROLE;

  SELECT count(*) INTO n FROM event_subscriptions WHERE source_id = drive_id;
  IF n <> 1 THEN
    RAISE EXCEPTION 'forgetting removed the row; the record of what we once '
      'subscribed to is part of the audit trail';
  END IF;

  -- -----------------------------------------------------------------
  -- Removing a source removes its subscription row with it
  -- -----------------------------------------------------------------
  -- This is why there is no "orphan subscriptions" query: source_id is
  -- ON DELETE CASCADE, so a subscription row cannot outlive its source. The
  -- orphan that CAN exist is one live in Google with no row here, and that is
  -- found by listing subscriptions from Google — see Reconciler.adopt.
  INSERT INTO event_sources (kind, external_id, display_name, visibility, rationale)
  VALUES ('drive', 'temporary-for-test', 'Temporary', 'internal', 'deleted below')
  RETURNING id INTO ghost;
  PERFORM system_record_subscription(ghost, 'subscriptions/temp', 'active',
                                     now() + interval '3 days', '["x"]'::jsonb, NULL);
  DELETE FROM event_sources WHERE id = ghost;

  SELECT count(*) INTO n FROM event_subscriptions WHERE google_name = 'subscriptions/temp';
  IF n <> 0 THEN
    RAISE EXCEPTION 'deleting a source left % subscription row(s) behind; the '
      'cascade is what makes a database orphan impossible', n;
  END IF;

  -- -----------------------------------------------------------------
  -- Receipts are deduplicated by Pub/Sub message id
  -- -----------------------------------------------------------------
  SET LOCAL ROLE workgraph_app;
  IF NOT system_record_event('msg-1', 'google.workspace.drive.file.v3.created',
                             '//drive.googleapis.com/drives/X', '{"a":1}'::jsonb) THEN
    RAISE EXCEPTION 'the first delivery of msg-1 was not recorded';
  END IF;
  IF system_record_event('msg-1', 'google.workspace.drive.file.v3.created',
                         '//drive.googleapis.com/drives/X', '{"a":1}'::jsonb) THEN
    RAISE EXCEPTION 'msg-1 was recorded twice; Pub/Sub redelivers, so this is '
      'the duplicate effect the acceptance criterion forbids';
  END IF;

  -- -----------------------------------------------------------------
  -- A delivery from an allow-listed source RESOLVES to that source
  -- -----------------------------------------------------------------
  -- This is the assertion that makes the "unknown source" one below mean
  -- anything. Until 2026-08-31 every real delivery resolved to NULL, because
  -- Google's ce-subject names the FILE and the drive appears nowhere — so the
  -- unknown-source test passed whether the allow-list worked or not.
  PERFORM system_record_subscription(meet_id, 'subscriptions/meet-spaces-abc', 'active',
                                     now() + interval '3 days',
                                     '["google.workspace.meet.transcript.v2.ended"]'::jsonb, NULL);
  IF NOT system_record_event('msg-attributed',
                             'google.workspace.meet.transcript.v2.ended',
                             'googleapis.com/meet/v2/conferenceRecords/xyz',
                             '{}'::jsonb,
                             'workspaceevents.googleapis.com/subscriptions/meet-spaces-abc') THEN
    RAISE EXCEPTION 'the attributed delivery was not recorded';
  END IF;
  RESET ROLE;

  SELECT count(*) INTO n FROM event_receipts
   WHERE message_id = 'msg-attributed' AND source_id = meet_id;
  IF n <> 1 THEN
    RAISE EXCEPTION 'a delivery naming an allow-listed source''s subscription '
      'resolved to no source; every event would be unattributable and the '
      'unknown-source assertion below would be unfalsifiable';
  END IF;

  -- The subscription is kept for evidence, not only for the join: "which
  -- subscription produced this" is the first question asked when deliveries stop.
  SELECT count(*) INTO n FROM event_receipts
   WHERE message_id = 'msg-attributed' AND subscription IS NULL;
  IF n <> 0 THEN
    RAISE EXCEPTION 'the producing subscription was not recorded';
  END IF;

  -- And an unknown subscription still resolves to nothing.
  SET LOCAL ROLE workgraph_app;
  PERFORM system_record_event('msg-stray', 'google.workspace.drive.file.v3.created',
                              'googleapis.com/drive/v3/files/whatever', '{}'::jsonb,
                              'workspaceevents.googleapis.com/subscriptions/not-ours');
  RESET ROLE;
  SELECT count(*) INTO n FROM event_receipts
   WHERE message_id = 'msg-stray' AND source_id IS NOT NULL;
  IF n <> 0 THEN
    RAISE EXCEPTION 'a delivery from a subscription we do not hold was attributed to a source';
  END IF;
END $$;

ROLLBACK;
