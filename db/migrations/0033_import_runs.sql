-- Staleness must mean "the importer is behind", not "the gateway is quiet"
-- (wg-qw1).
--
-- system_budget_status measured staleness as the age of the newest usage record.
-- That is wrong in a way that only shows up on a healthy system: a gateway
-- nobody has used for a week has a newest record a week old, however punctually
-- the importer has run. The budget check would then refuse every dispatch with
--
--   spend data is 216h49m old, older than the 2h this check trusts
--
-- which is true of the DATA and says nothing about whether it is current. The
-- quieter the environment, the more certainly it blocks work.
--
-- Observed immediately: staging's gateways have had no traffic since
-- 2026-08-17, so the first budget check after wiring up the importer refused for
-- exactly this reason while the importer was running correctly every hour.
--
-- What the caller actually needs to know is whether the numbers it is reading
-- are up to date, which is a fact about the IMPORT, not about the spend. So the
-- import records its own passes and staleness is measured from those.
--
-- The bead that tracks alerting on a stopped importer (wg-7jz) had already
-- written down this distinction — "it likely keys off the last successful RUN
-- rather than the newest ENTRY" — and this is the same realisation arriving from
-- the other direction.

BEGIN;

CREATE TABLE IF NOT EXISTS usage_import_runs (
    -- One row per gateway, overwritten. The history of import runs is not
    -- interesting; whether the last one was recent is.
    gateway      text PRIMARY KEY,
    ran_at       timestamptz NOT NULL DEFAULT now(),
    entries_seen bigint NOT NULL DEFAULT 0,
    -- False when the pass stopped at its per-run cap rather than reaching the
    -- start of its window. Such a run has NOT caught up, and a budget decision
    -- resting on it is resting on a partial read.
    complete     boolean NOT NULL DEFAULT true
);

-- Deliberately NOT row-level-security protected, and worth saying why: it holds
-- no project data, only "gateway X was read at time T". Adding a policy would
-- mean the importer needed a user to write it and the budget check needed one to
-- read it, for a row that describes the platform rather than anybody's work.

CREATE OR REPLACE FUNCTION system_record_import_run(
    p_gateway  text,
    p_entries  bigint DEFAULT 0,
    p_complete boolean DEFAULT true
) RETURNS void
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    INSERT INTO usage_import_runs (gateway, ran_at, entries_seen, complete)
    VALUES (p_gateway, now(), coalesce(p_entries, 0), coalesce(p_complete, true))
    ON CONFLICT (gateway) DO UPDATE
       SET ran_at = now(),
           entries_seen = EXCLUDED.entries_seen,
           complete = EXCLUDED.complete;
$$;

REVOKE ALL ON FUNCTION system_record_import_run(text, bigint, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_record_import_run(text, bigint, boolean) TO workgraph_app;

-- Same signature and same columns as 0030; only the staleness changes, from the
-- age of the newest record to the age of the least recent import.
--
-- min(ran_at), not max: the question is whether ALL the spend is current, and
-- one gateway that has not been read for a day makes the total wrong however
-- promptly the others were.
CREATE OR REPLACE FUNCTION system_budget_status(p_bead text, p_cell text DEFAULT NULL)
RETURNS TABLE (
    subject_kind   text,
    subject_key    text,
    daily_cents    numeric,
    spent_cents    numeric,
    remaining_cents numeric,
    exceeded       boolean,
    newest_record_at timestamptz,
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

    IF v_work_ref IS NOT NULL THEN
        SELECT b.daily_cost_cents INTO v_limit FROM budget_limits b WHERE b.work_ref_id = v_work_ref;
        IF v_limit IS NOT NULL THEN
            v_kind := 'bead'; v_key := p_bead;
            SELECT coalesce(sum(u.cost_cents), 0) INTO v_spent
              FROM usage_records u WHERE u.bead = p_bead AND u.occurred_at >= v_day;
        END IF;
    END IF;

    IF v_limit IS NULL AND v_project IS NOT NULL THEN
        SELECT b.daily_cost_cents INTO v_limit FROM budget_limits b WHERE b.project_id = v_project;
        IF v_limit IS NOT NULL THEN
            v_kind := 'project';
            SELECT p.slug INTO v_key FROM projects p WHERE p.id = v_project;
            SELECT coalesce(sum(u.cost_cents), 0) INTO v_spent
              FROM usage_records u WHERE u.project_id = v_project AND u.occurred_at >= v_day;
        END IF;
    END IF;

    IF v_limit IS NULL AND v_cell IS NOT NULL THEN
        SELECT b.daily_cost_cents INTO v_limit FROM budget_limits b WHERE b.execution_cell_id = v_cell;
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
                 ELSE floor(EXTRACT(EPOCH FROM (now() - v_imported)))::bigint END;
        RETURN;
    END IF;

    RETURN QUERY SELECT
        v_kind, v_key, v_limit, v_spent, v_limit - v_spent, v_spent >= v_limit,
        v_newest,
        CASE WHEN v_imported IS NULL THEN NULL
             ELSE floor(EXTRACT(EPOCH FROM (now() - v_imported)))::bigint END;
END
$$;

COMMIT;
