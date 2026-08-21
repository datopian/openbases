-- Delivery and clearing for platform alerts (WP-I1).
--
-- The monitor needs two things the schema does not provide.
--
-- FIRST, somewhere to file an alert that belongs to no project. Every existing
-- inbox path goes through system_attention_recipients, which resolves a PROJECT's
-- leads and falls back to the organisation admins — and which deliberately
-- returns NOBODY for a slug matching no project, because "this escalation found
-- nobody" is how an unregistered repository gets noticed. That is right for agent
-- escalations and wrong for "the disk is full", which is nobody's project work.
--
-- The first attempt here was to invent a workgraph-platform project to hang the
-- alerts on. That was the wrong shape, and the schema said so: projects require
-- an organisation, a primary owner and a backup owner that differ, because
-- full-cycle ownership must not be a single point of failure. Satisfying those
-- columns would have meant inventing an owner for the platform — an
-- organisational decision, taken silently by a migration, to make a foreign key
-- happy. attention_items.project_id is already nullable, so a platform alert can
-- simply not have a project, which is the truth.
--
-- SECOND, a way for an alert to go away. system_raise_attention upserts while an
-- item is open or snoozed, which keeps an ongoing problem to one refreshed item
-- rather than one per cycle. Nothing closes it when the condition clears, so an
-- operator would resolve every alert by hand — including the ones whose cause
-- fixed itself, which is exactly how people learn to stop reading an inbox.

BEGIN;

-- Raise (or refresh) a platform alert for every organisation admin.
--
-- Recipients are the organisation admins directly. There is no project to derive
-- a lead from, and the admins are already the fallback the rest of the system
-- uses when a project has nobody assigned, so this does not invent a new
-- escalation audience.
--
-- Returns the number of inboxes reached. ZERO is a meaningful answer and the
-- caller is expected to treat it as a failure: an alert that reached nobody looks
-- exactly like an alert that was handled.
CREATE OR REPLACE FUNCTION system_raise_platform_alert(
    p_rule        text,
    p_dedupe_key  text,
    p_score       numeric,
    p_explanation jsonb
) RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_count integer;
BEGIN
    IF p_dedupe_key IS NULL OR p_dedupe_key NOT LIKE 'monitor:%' THEN
        RAISE EXCEPTION 'a platform alert must carry a monitor: dedupe key, got %', p_dedupe_key;
    END IF;

    INSERT INTO attention_items
        (user_id, project_id, rule, score, explanation, visibility, dedupe_key, last_seen_at)
    SELECT g.user_id, NULL, p_rule, p_score, p_explanation, 'internal', p_dedupe_key, now()
      FROM role_grants g
     WHERE g.role_name = 'organisation_admin'
       AND g.project_id IS NULL
    ON CONFLICT (user_id, dedupe_key)
        WHERE dedupe_key IS NOT NULL AND status IN ('open', 'snoozed')
    DO UPDATE SET
        -- Refreshed, because the situation moves: 86% full and 97% full are the
        -- same alert and very different urgencies. Status is left alone so a
        -- snooze the operator set is not undone by the machine noticing again.
        explanation  = EXCLUDED.explanation,
        score        = EXCLUDED.score,
        last_seen_at = now();

    GET DIAGNOSTICS v_count = ROW_COUNT;
    RETURN v_count;
END
$$;

-- Clear an alert whose condition has gone away.
--
-- Scoped to the monitor's own keys by the 'monitor:%' prefix, so this can never
-- resolve an approval, a narrative or an agent escalation. A function that could
-- close any inbox item would be a way to make an inconvenient approval request
-- disappear.
--
-- Snoozed and delegated items are resolved too: the operator snoozed a real
-- problem, the problem is gone, and letting it reappear when the snooze expires
-- would be reporting history as news.
CREATE OR REPLACE FUNCTION system_resolve_platform_alert(p_dedupe_key text)
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_count integer;
BEGIN
    IF p_dedupe_key IS NULL OR p_dedupe_key NOT LIKE 'monitor:%' THEN
        RAISE EXCEPTION 'system_resolve_platform_alert only resolves monitor alerts, not %', p_dedupe_key;
    END IF;

    UPDATE attention_items
       SET status = 'resolved', resolved_at = now()
     WHERE dedupe_key = p_dedupe_key
       AND status IN ('open', 'snoozed', 'delegated');

    GET DIAGNOSTICS v_count = ROW_COUNT;
    RETURN v_count;
END
$$;

REVOKE ALL ON FUNCTION system_raise_platform_alert(text, text, numeric, jsonb) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_resolve_platform_alert(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_raise_platform_alert(text, text, numeric, jsonb) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_resolve_platform_alert(text) TO workgraph_app;

-- ---------------------------------------------------------------------------
-- The system path for the monitor's reads
-- ---------------------------------------------------------------------------
--
-- github_deliveries and agent_health_events both have row-level security enabled
-- with NO policy at all (0014, 0015). That is deliberate — those rows are raw
-- payloads and infrastructure facts that no user session has a reason to read —
-- and it means workgraph_app sees an EMPTY table, not a filtered one.
--
-- A monitor querying them directly would therefore report "0 deliveries
-- unprocessed" and "no agent-health report has ever arrived" for ever, whatever
-- was actually happening. The first of those reads as healthy. This is not a
-- hypothesis: 0014 records the same mistake being made and found the hard way —
-- "the first reconciliation pass reported zero repositories resynced and zero
-- failures, a silent no-op, because project_repositories is protected and the
-- pass had no identity."
--
-- So the monitor gets the same treatment as reconciliation: narrow SECURITY
-- DEFINER functions that return only what it needs. And what it needs is
-- AGGREGATES — a count and two timestamps. Nothing here can return a commit
-- message, a branch name or a bead id, so the monitor cannot become a way to
-- read restricted repository activity even though it runs unattended with no
-- user identity.

-- How far behind the inbound delivery queue is.
CREATE OR REPLACE FUNCTION system_webhook_backlog()
RETURNS TABLE (unprocessed bigint, oldest_seconds numeric)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT count(*),
           COALESCE(EXTRACT(EPOCH FROM (now() - min(received_at))), 0)::numeric
      FROM github_deliveries
     WHERE processed_at IS NULL;
$$;

-- When the witness last reported anything at all.
--
-- NULL means never, which the monitor treats as a failure when cells are
-- deployed: silence from the component whose job is to report trouble looks
-- exactly like good news.
CREATE OR REPLACE FUNCTION system_agent_health_last_seen()
RETURNS timestamptz
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT max(observed_at) FROM agent_health_events;
$$;

REVOKE ALL ON FUNCTION system_webhook_backlog() FROM PUBLIC;
REVOKE ALL ON FUNCTION system_agent_health_last_seen() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_webhook_backlog() TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_agent_health_last_seen() TO workgraph_app;

COMMIT;
