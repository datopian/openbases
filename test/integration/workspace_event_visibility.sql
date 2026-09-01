-- Who can read a Workspace event, and who cannot (WP-H3).
--
-- The read path added in 0051 is the first time anything reachable by a person
-- could read event_receipts: before it, those tables had row-level security
-- forced with zero policies. So the question "does a restricted project's
-- meeting leak" goes from unanswerable to answerable, and this answers it.
--
-- The interesting case is not an outsider. It is the lead of the OTHER client
-- engagement: two restricted projects now exist, and a leak between two clients
-- is worse than a leak to an employee.

BEGIN;

DO $$
DECLARE org uuid;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  -- Resolved and stashed while still running as the owner. Read after the role
  -- drop these come back NULL, because `projects` is behind RLS with no
  -- identity set -- and every assertion below then passes for the wrong reason.
  PERFORM set_config('test.cdt_lead',
    (SELECT primary_owner_id::text FROM projects WHERE slug = 'cdt'), false);
  PERFORM set_config('test.nged_lead',
    (SELECT primary_owner_id::text FROM projects WHERE slug = 'nged'), false);
  PERFORM set_config('test.admin',
    (SELECT user_id::text FROM role_grants
      WHERE role_name = 'organisation_admin' AND project_id IS NULL LIMIT 1), false);

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Events Outsider', 'events-outsider@datopian.com');
  PERFORM set_config('test.outsider',
    (SELECT id::text FROM users WHERE primary_email = 'events-outsider@datopian.com'), false);

  -- A receipt against the CDT meeting, and one against a shared drive, so the
  -- two visibility shapes are both exercised.
  INSERT INTO event_receipts (message_id, source_id, event_type, target, subscription, payload)
  SELECT 'vis-cdt-1', s.id, 'google.workspace.meet.transcript.v2.ended',
         'googleapis.com/meet/v2/conferenceRecords/vis', 'subscriptions/vis', '{}'::jsonb
    FROM event_sources s WHERE s.kind = 'meet' AND s.project_id = (SELECT id FROM projects WHERE slug = 'cdt')
  ON CONFLICT (message_id) DO NOTHING;

  INSERT INTO event_receipts (message_id, source_id, event_type, target, subscription, payload)
  SELECT 'vis-drive-1', s.id, 'google.workspace.drive.file.v3.created',
         'googleapis.com/drive/v3/files/vis', 'subscriptions/vis-d', '{}'::jsonb
    FROM event_sources s WHERE s.kind = 'drive' AND s.display_name = 'All'
  ON CONFLICT (message_id) DO NOTHING;

  -- A delivery from a source nobody allow-listed.
  INSERT INTO event_receipts (message_id, source_id, event_type, target, payload)
  VALUES ('vis-orphan-1', NULL, 'google.workspace.drive.file.v3.created',
          'googleapis.com/drive/v3/files/stranger', '{}'::jsonb)
  ON CONFLICT (message_id) DO NOTHING;
END $$;

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE n integer;
BEGIN
  -- The CDT lead sees the CDT meeting.
  PERFORM set_config('workgraph.user_id', current_setting('test.cdt_lead'), true);
  SELECT count(*) INTO n FROM event_receipts WHERE message_id = 'vis-cdt-1';
  IF n <> 1 THEN RAISE EXCEPTION 'the cdt lead cannot see their own project''s meeting'; END IF;

  -- THE CASE THAT MATTERS. The lead of the other client engagement must not.
  PERFORM set_config('workgraph.user_id', current_setting('test.nged_lead'), true);
  SELECT count(*) INTO n FROM event_receipts WHERE message_id = 'vis-cdt-1';
  IF n <> 0 THEN
    RAISE EXCEPTION 'the nged lead can read a CDT meeting event -- one client''s '
      'material is visible to another';
  END IF;
  SELECT count(*) INTO n FROM event_sources
   WHERE kind = 'meet' AND display_name = 'CDT internal kick-off';
  IF n <> 0 THEN
    RAISE EXCEPTION 'the nged lead can see that the CDT kick-off source exists';
  END IF;

  -- An employee on no project sees no project-scoped event either.
  PERFORM set_config('workgraph.user_id', current_setting('test.outsider'), true);
  SELECT count(*) INTO n FROM event_receipts WHERE message_id = 'vis-cdt-1';
  IF n <> 0 THEN RAISE EXCEPTION 'a non-member read a restricted project''s event'; END IF;

  -- But the shared drives are 'internal' with no project, which is what that
  -- has always meant: any authenticated person may see them. Asserted so the
  -- policy is understood rather than assumed, and so tightening it later is a
  -- deliberate change to this line.
  SELECT count(*) INTO n FROM event_receipts WHERE message_id = 'vis-drive-1';
  IF n <> 1 THEN
    RAISE EXCEPTION 'an authenticated user cannot see a company-wide drive event';
  END IF;

  -- A delivery with no source stays invisible to everyone. Its target names a
  -- resource nobody has classified, so there is no level at which to show it.
  SELECT count(*) INTO n FROM event_receipts WHERE message_id = 'vis-orphan-1';
  IF n <> 0 THEN RAISE EXCEPTION 'an unattributed delivery is readable'; END IF;

  PERFORM set_config('workgraph.user_id', current_setting('test.admin'), true);
  SELECT count(*) INTO n FROM event_receipts WHERE message_id = 'vis-orphan-1';
  IF n <> 0 THEN
    RAISE EXCEPTION 'an unattributed delivery is readable by an organisation admin';
  END IF;

  -- An organisation admin sees the restricted project's events, because the
  -- grant satisfies can_read_project. Stated explicitly: it is a real decision
  -- that org-wide grants reach a restricted client engagement, and it is the
  -- same rule that already governs the project row itself.
  SELECT count(*) INTO n FROM event_receipts WHERE message_id = 'vis-cdt-1';
  IF n <> 1 THEN
    RAISE EXCEPTION 'an organisation admin cannot see a restricted project''s events';
  END IF;

  -- And with no identity at all.
  PERFORM set_config('workgraph.user_id', '', true);
  SELECT count(*) INTO n FROM event_receipts;
  IF n <> 0 THEN RAISE EXCEPTION 'an unidentified session read % receipt(s)', n; END IF;

  RAISE NOTICE 'workspace event visibility assertions passed';
END $$;

ROLLBACK;
