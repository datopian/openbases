-- Return the operational limits a budget carries, so they can be enforced
-- (wg-726).
--
-- budget_limits has held max_concurrent_agents and max_runtime_minutes since
-- 0006. wg-budget sets them, the table stores them, and nothing has ever read
-- them: the only runtime ceiling in force is agent_max_runtime_minutes in the
-- execution_cell role, one node-wide value on a systemd timer that knows nothing
-- about which project or bead is running, and concurrency is not enforced at
-- all — the cell slice sets TasksMax, which bounds processes rather than agents.
--
-- Two numbers that look like controls and are not. Cost was the only dimension
-- of ADR-0022 that actually enforced.
--
-- They are returned alongside the spend rather than through a second function,
-- for the same reason resolution and summation live together in 0030: a caller
-- deciding whether to dispatch needs one answer about one subject, and two
-- lookups are two chances to resolve to different subjects.

BEGIN;

DROP FUNCTION IF EXISTS system_budget_status(text, text);

CREATE OR REPLACE FUNCTION system_budget_status(p_bead text, p_cell text DEFAULT NULL)
RETURNS TABLE (
    subject_kind   text,
    subject_key    text,
    daily_cents    numeric,
    spent_cents    numeric,
    remaining_cents numeric,
    exceeded       boolean,
    newest_record_at timestamptz,
    staleness_seconds bigint,
    max_agents     integer,
    max_runtime_minutes integer
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_work_ref uuid;
    v_project  uuid;
    v_cell     uuid;
    v_kind     text;
    v_key      text;
    v_limit    numeric;
    v_spent    numeric := 0;
    v_agents   integer;
    v_runtime  integer;
    v_day      timestamptz := date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    v_newest   timestamptz;
    v_imported timestamptz;
BEGIN
    SELECT id, project_id INTO v_work_ref, v_project
      FROM work_refs WHERE bead_id = p_bead LIMIT 1;

    IF p_cell IS NOT NULL AND p_cell <> '' THEN
        SELECT id INTO v_cell FROM execution_cells WHERE slug = p_cell;
        IF v_project IS NULL AND v_cell IS NOT NULL THEN
            SELECT p.id INTO v_project FROM projects p
             WHERE p.execution_cell_id = v_cell LIMIT 1;
        END IF;
    END IF;

    -- Most specific first: bead, then project, then cell. The operational
    -- limits come from the SAME row as the ceiling, so a bead budget's
    -- concurrency governs where a bead budget governs the money.
    IF v_work_ref IS NOT NULL THEN
        SELECT b.daily_cost_cents, b.max_concurrent_agents, b.max_runtime_minutes
          INTO v_limit, v_agents, v_runtime
          FROM budget_limits b WHERE b.work_ref_id = v_work_ref;
        IF v_limit IS NOT NULL THEN
            v_kind := 'bead'; v_key := p_bead;
            SELECT coalesce(sum(u.cost_cents), 0) INTO v_spent
              FROM usage_records u WHERE u.bead = p_bead AND u.occurred_at >= v_day;
        END IF;
    END IF;

    IF v_limit IS NULL AND v_project IS NOT NULL THEN
        SELECT b.daily_cost_cents, b.max_concurrent_agents, b.max_runtime_minutes
          INTO v_limit, v_agents, v_runtime
          FROM budget_limits b WHERE b.project_id = v_project;
        IF v_limit IS NOT NULL THEN
            v_kind := 'project';
            SELECT p.slug INTO v_key FROM projects p WHERE p.id = v_project;
            SELECT coalesce(sum(u.cost_cents), 0) INTO v_spent
              FROM usage_records u WHERE u.project_id = v_project AND u.occurred_at >= v_day;
        END IF;
    END IF;

    IF v_limit IS NULL AND v_cell IS NOT NULL THEN
        SELECT b.daily_cost_cents, b.max_concurrent_agents, b.max_runtime_minutes
          INTO v_limit, v_agents, v_runtime
          FROM budget_limits b WHERE b.execution_cell_id = v_cell;
        IF v_limit IS NOT NULL THEN
            v_kind := 'cell'; v_key := p_cell;
            SELECT coalesce(sum(u.cost_cents), 0) INTO v_spent
              FROM usage_records u WHERE u.cell = p_cell AND u.occurred_at >= v_day;
        END IF;
    END IF;

    SELECT max(u.occurred_at) INTO v_newest FROM usage_records u;
    SELECT min(r.ran_at) INTO v_imported FROM usage_import_runs r;

    IF v_limit IS NULL THEN
        RETURN QUERY SELECT
            'none'::text, NULL::text, NULL::numeric, v_spent, NULL::numeric, false,
            v_newest,
            CASE WHEN v_imported IS NULL THEN NULL
                 ELSE floor(EXTRACT(EPOCH FROM (now() - v_imported)))::bigint END,
            NULL::integer, NULL::integer;
        RETURN;
    END IF;

    RETURN QUERY SELECT
        v_kind, v_key, v_limit, v_spent, v_limit - v_spent, v_spent >= v_limit,
        v_newest,
        CASE WHEN v_imported IS NULL THEN NULL
             ELSE floor(EXTRACT(EPOCH FROM (now() - v_imported)))::bigint END,
        v_agents, v_runtime;
END
$$;

REVOKE ALL ON FUNCTION system_budget_status(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_budget_status(text, text) TO workgraph_app;

COMMIT;
