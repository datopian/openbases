-- Platform alert delivery and clearing (WP-I1).
--
-- The acceptance criterion for this work package is not "an alert is computed".
-- It is that the alert REACHES the designated operator and carries a runbook
-- link. Those are properties of the delivery path, so they are asserted here
-- against the database rather than in Go against a mock — a mock would prove the
-- code calls a function, not that the function reaches anybody.
--
-- Runs as the owner deliberately: nothing here asserts visibility. It asserts who
-- receives an alert, that repeats refresh rather than accumulate, that a snooze
-- survives, and that the resolve path cannot be used to close somebody else's
-- approval request.

BEGIN;

DO $$
DECLARE
    admin_a  uuid;
    admin_b  uuid;
    outsider uuid;
    n        integer;
    expected integer;
    v_expl   jsonb;
    v_status text;
BEGIN
    -- The organisation admins are the audience for a platform alert, so the
    -- assertions below are relative to however many this database has rather
    -- than to a number written here. A seed change should not silently turn this
    -- test into one that asserts nothing.
    SELECT count(*) INTO expected
      FROM role_grants WHERE role_name = 'organisation_admin' AND project_id IS NULL;
    IF expected = 0 THEN
        RAISE EXCEPTION 'no organisation admin exists, so every platform alert '
                        'would reach nobody and this test would pass vacuously';
    END IF;

    SELECT g.user_id INTO admin_a
      FROM role_grants g WHERE g.role_name = 'organisation_admin' AND g.project_id IS NULL
     ORDER BY g.user_id LIMIT 1;

    -- 1. An alert reaches every organisation admin, and only them.
    n := system_raise_platform_alert(
        'platform_disk_pressure', 'monitor:disk', 0.8,
        '{"check":"disk","summary":"/ is 91.0% full","runbook":"docs/runbooks/disk-pressure.md"}'::jsonb);
    IF n <> expected THEN
        RAISE EXCEPTION 'the alert reached % inbox(es), expected % organisation admin(s)', n, expected;
    END IF;

    SELECT count(*) INTO n FROM attention_items WHERE dedupe_key = 'monitor:disk';
    IF n <> expected THEN
        RAISE EXCEPTION 'expected % inbox item(s), found %', expected, n;
    END IF;

    -- 2. It carries the runbook. An alert that says something is wrong and not
    --    what to do about it is a notification, not an alert, and this is the
    --    criterion the work package is judged on.
    SELECT explanation INTO v_expl
      FROM attention_items WHERE dedupe_key = 'monitor:disk' AND user_id = admin_a;
    IF v_expl->>'runbook' IS NULL OR v_expl->>'runbook' = '' THEN
        RAISE EXCEPTION 'the delivered alert carries no runbook link: %', v_expl;
    END IF;
    IF v_expl->>'summary' IS NULL THEN
        RAISE EXCEPTION 'the delivered alert carries no summary: %', v_expl;
    END IF;

    -- 3. A platform alert has no project. It is not project work, and hanging it
    --    on one would have meant inventing an owner for the platform.
    SELECT count(*) INTO n
      FROM attention_items WHERE dedupe_key = 'monitor:disk' AND project_id IS NOT NULL;
    IF n <> 0 THEN
        RAISE EXCEPTION '% platform alert(s) were filed against a project', n;
    END IF;

    -- 4. A repeat REFRESHES rather than accumulating. A monitor running every
    --    five minutes through a two-hour outage must not produce 24 items.
    n := system_raise_platform_alert(
        'platform_disk_pressure', 'monitor:disk', 0.9,
        '{"check":"disk","summary":"/ is 97.0% full","runbook":"docs/runbooks/disk-pressure.md"}'::jsonb);
    SELECT count(*) INTO n FROM attention_items WHERE dedupe_key = 'monitor:disk';
    IF n <> expected THEN
        RAISE EXCEPTION 'a repeated alert produced % items, expected the original %', n, expected;
    END IF;

    -- And the refresh carries the CURRENT situation. 91% and 97% are the same
    -- alert and very different urgencies; a stale explanation would send an
    -- operator to a problem they think they understand.
    SELECT explanation INTO v_expl
      FROM attention_items WHERE dedupe_key = 'monitor:disk' AND user_id = admin_a;
    IF v_expl->>'summary' NOT LIKE '%97.0%' THEN
        RAISE EXCEPTION 'the refreshed alert still shows the old situation: %', v_expl->>'summary';
    END IF;

    -- 5. A snooze the operator set is NOT undone by the machine noticing again.
    UPDATE attention_items
       SET status = 'snoozed', snoozed_until = now() + interval '4 hours'
     WHERE dedupe_key = 'monitor:disk' AND user_id = admin_a;

    n := system_raise_platform_alert(
        'platform_disk_pressure', 'monitor:disk', 0.9,
        '{"check":"disk","summary":"still full","runbook":"docs/runbooks/disk-pressure.md"}'::jsonb);

    SELECT status INTO v_status
      FROM attention_items WHERE dedupe_key = 'monitor:disk' AND user_id = admin_a;
    IF v_status <> 'snoozed' THEN
        RAISE EXCEPTION 'raising the alert again un-snoozed it (status now %)', v_status;
    END IF;

    -- 6. Clearing works, and clears the snoozed one too — the operator snoozed a
    --    real problem and the problem is gone.
    n := system_resolve_platform_alert('monitor:disk');
    IF n <> expected THEN
        RAISE EXCEPTION 'resolving closed % item(s), expected %', n, expected;
    END IF;
    SELECT count(*) INTO n
      FROM attention_items
     WHERE dedupe_key = 'monitor:disk' AND status <> 'resolved';
    IF n <> 0 THEN
        RAISE EXCEPTION '% alert(s) survived being resolved', n;
    END IF;

    -- 7. Once resolved, the next occurrence is a NEW alert rather than being
    --    swallowed. The upsert only matches open and snoozed rows, and if it
    --    matched resolved ones a recurring fault would be reported once, ever.
    n := system_raise_platform_alert(
        'platform_disk_pressure', 'monitor:disk', 0.8,
        '{"check":"disk","summary":"full again","runbook":"docs/runbooks/disk-pressure.md"}'::jsonb);
    IF n <> expected THEN
        RAISE EXCEPTION 'a recurrence after resolution reached % inbox(es), expected %', n, expected;
    END IF;
    SELECT count(*) INTO n
      FROM attention_items WHERE dedupe_key = 'monitor:disk' AND status = 'open';
    IF n <> expected THEN
        RAISE EXCEPTION 'the recurrence did not produce an open item (% found)', n;
    END IF;

    -- 8. Resolving is scoped to the monitor's own keys. Without the prefix check
    --    this function would be a way to make an inconvenient approval request
    --    disappear, called by the same role the API runs as.
    BEGIN
        n := system_resolve_platform_alert('approval:something-expensive');
        RAISE EXCEPTION 'a non-monitor dedupe key was accepted for resolution';
    EXCEPTION WHEN raise_exception THEN
        IF SQLERRM = 'a non-monitor dedupe key was accepted for resolution' THEN
            RAISE;
        END IF;
    END;

    -- 9. And raising is scoped the same way, so nothing can mint an alert that
    --    the resolve path is then unable to clear.
    BEGIN
        n := system_raise_platform_alert('platform_disk_pressure', 'approval:sneaky', 0.8, '{}'::jsonb);
        RAISE EXCEPTION 'a non-monitor dedupe key was accepted for raising';
    EXCEPTION WHEN raise_exception THEN
        IF SQLERRM = 'a non-monitor dedupe key was accepted for raising' THEN
            RAISE;
        END IF;
    END;

    RAISE NOTICE 'platform alert delivery OK (% organisation admin(s) reached)', expected;
END $$;

-- ---------------------------------------------------------------------------
-- The monitor's reads must actually see something
-- ---------------------------------------------------------------------------
--
-- github_deliveries and agent_health_events have row-level security enabled with
-- NO policy, so the application role sees an EMPTY table rather than a filtered
-- one. A monitor querying them directly would report "0 deliveries unprocessed"
-- and "no agent-health report ever arrived" for ever — and the first of those
-- reads as healthy, which is the worst possible failure for a detection system.
--
-- This section is the regression guard. It asserts both halves: that the tables
-- really are invisible to the application role, and that the system functions
-- really do see through. Asserting only the second would still pass if somebody
-- "fixed" the problem by adding a permissive policy, which would quietly expose
-- raw payloads from restricted repositories to every member.

-- Seeded as the owner, because there is no INSERT policy either.
INSERT INTO github_deliveries (delivery_id, event_type, received_at, processed_at, payload)
VALUES ('platform-alert-test', 'push', now() - interval '2 hours', NULL, '{}'::jsonb);

SELECT system_record_agent_health('acc-cell', 'rig', 'pc', 'observe', 'r', 'b', NULL, false);

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE
    n        bigint;
    oldest   numeric;
    last_at  timestamptz;
BEGIN
    -- Invisible directly.
    SELECT count(*) INTO n FROM github_deliveries;
    IF n <> 0 THEN
        RAISE EXCEPTION 'the application role can read % github_deliveries row(s) directly; '
                        'raw payloads from restricted repositories are exposed', n;
    END IF;
    SELECT count(*) INTO n FROM agent_health_events;
    IF n <> 0 THEN
        RAISE EXCEPTION 'the application role can read % agent_health_events row(s) directly', n;
    END IF;

    -- Visible through the system path, which is what the monitor uses.
    SELECT unprocessed, oldest_seconds INTO n, oldest FROM system_webhook_backlog();
    IF n < 1 THEN
        RAISE EXCEPTION 'system_webhook_backlog() reported % unprocessed deliveries; the '
                        'monitor would report a healthy queue while one is stuck', n;
    END IF;
    IF oldest < 3600 THEN
        RAISE EXCEPTION 'system_webhook_backlog() aged the oldest delivery at %s, expected '
                        'about two hours', oldest;
    END IF;

    last_at := system_agent_health_last_seen();
    IF last_at IS NULL THEN
        RAISE EXCEPTION 'system_agent_health_last_seen() returned NULL with an event present; '
                        'the monitor would report the witness dead for ever';
    END IF;

    RAISE NOTICE 'the monitor system path sees through RLS while the application role does not';
END $$;

RESET ROLE;

ROLLBACK;
