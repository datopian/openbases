-- What a bead is allowed to spend, and what it has spent (wg-qw1).
--
-- One function, because the resolution order and the summation have to agree.
-- If "which budget applies" lived in one place and "what counts against it" in
-- another, they would drift and the first symptom would be a budget that never
-- trips or one that always does.
--
-- Resolution is bead, then project, then cell: the most specific budget that
-- exists wins, and spend is summed at that SAME level. That pairing is the whole
-- design. A bead budget compares bead spend; a project budget compares the
-- project's spend across all its beads; a cell budget compares everything that
-- ran in the cell. Summing bead spend against a project ceiling would let ten
-- beads each spend the project's whole budget.
--
-- The staleness is returned rather than hidden. Spend arrives through an hourly
-- import, so this answer is always somewhat behind, and a caller that treats an
-- hour-old "you have $3 left" as current will authorise work that is already
-- over. The age of the newest imported record is part of the answer so the
-- decision can say so out loud.

BEGIN;

CREATE OR REPLACE FUNCTION system_budget_status(p_bead text, p_cell text DEFAULT NULL)
RETURNS TABLE (
    subject_kind   text,
    subject_key    text,
    daily_cents    numeric,
    spent_cents    numeric,
    remaining_cents numeric,
    exceeded       boolean,
    newest_record_at timestamptz,
    -- NULL when nothing has ever been imported, which is different from "no
    -- spend today" and must not be reported as freshness.
    staleness_seconds bigint
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
    v_day      timestamptz := date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    v_newest   timestamptz;
BEGIN
    SELECT id, project_id INTO v_work_ref, v_project
      FROM work_refs WHERE bead_id = p_bead LIMIT 1;

    IF p_cell IS NOT NULL AND p_cell <> '' THEN
        SELECT id INTO v_cell FROM execution_cells WHERE slug = p_cell;
        -- The cell also gives a project when the bead has not been seen yet,
        -- which is the common case for work that has just been filed.
        IF v_project IS NULL AND v_cell IS NOT NULL THEN
            SELECT p.id INTO v_project FROM projects p
             WHERE p.execution_cell_id = v_cell
             LIMIT 1;
        END IF;
    END IF;

    -- Most specific first.
    IF v_work_ref IS NOT NULL THEN
        SELECT b.daily_cost_cents INTO v_limit FROM budget_limits b WHERE b.work_ref_id = v_work_ref;
        IF v_limit IS NOT NULL THEN
            v_kind := 'bead'; v_key := p_bead;
            SELECT coalesce(sum(u.cost_cents), 0) INTO v_spent
              FROM usage_records u
             WHERE u.bead = p_bead AND u.occurred_at >= v_day;
        END IF;
    END IF;

    IF v_limit IS NULL AND v_project IS NOT NULL THEN
        SELECT b.daily_cost_cents INTO v_limit FROM budget_limits b WHERE b.project_id = v_project;
        IF v_limit IS NOT NULL THEN
            v_kind := 'project';
            SELECT p.slug INTO v_key FROM projects p WHERE p.id = v_project;
            SELECT coalesce(sum(u.cost_cents), 0) INTO v_spent
              FROM usage_records u
             WHERE u.project_id = v_project AND u.occurred_at >= v_day;
        END IF;
    END IF;

    IF v_limit IS NULL AND v_cell IS NOT NULL THEN
        SELECT b.daily_cost_cents INTO v_limit FROM budget_limits b WHERE b.execution_cell_id = v_cell;
        IF v_limit IS NOT NULL THEN
            v_kind := 'cell'; v_key := p_cell;
            SELECT coalesce(sum(u.cost_cents), 0) INTO v_spent
              FROM usage_records u
             WHERE u.cell = p_cell AND u.occurred_at >= v_day;
        END IF;
    END IF;

    SELECT max(u.occurred_at) INTO v_newest FROM usage_records u;

    IF v_limit IS NULL THEN
        -- No budget anywhere up the chain. Reported as a distinct answer rather
        -- than as an unlimited one: "nobody set a budget" and "the budget is
        -- large" want different responses from a caller, and conflating them is
        -- how an unbudgeted bead comes to look approved.
        RETURN QUERY SELECT
            'none'::text, NULL::text, NULL::numeric, v_spent, NULL::numeric, false,
            v_newest,
            CASE WHEN v_newest IS NULL THEN NULL
                 ELSE floor(EXTRACT(EPOCH FROM (now() - v_newest)))::bigint END;
        RETURN;
    END IF;

    RETURN QUERY SELECT
        v_kind, v_key, v_limit, v_spent, v_limit - v_spent, v_spent >= v_limit,
        v_newest,
        CASE WHEN v_newest IS NULL THEN NULL
             ELSE floor(EXTRACT(EPOCH FROM (now() - v_newest)))::bigint END;
END
$$;

REVOKE ALL ON FUNCTION system_budget_status(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_budget_status(text, text) TO workgraph_app;

COMMIT;
