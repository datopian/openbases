-- Per-bead budgets that can actually be enforced (wg-qw1, plan §4.7).
--
-- budget_limits existed with zero rows and nothing reading it. The only spend
-- ceiling that really existed was the per-gateway pool on Cloudflare, which
-- returns 429 when the pool is dry — a blunt instrument: per gateway, not per
-- project, and certainly not per piece of work.
--
-- Three things had to change before a budget could mean anything.
--
-- 1. It could not hold a real number. daily_cost_cents was integer, the same
--    defect 0024 fixed in usage_records. A whole-cent daily ceiling is workable
--    where a whole-cent per-request cost was not, but a budget that cannot
--    express $0.50 exactly is a budget people argue with instead of trusting.
--
-- 2. There was no per-bead subject. budget_has_subject allowed exactly one of
--    project_id or execution_cell_id, so "this piece of work may cost $2" could
--    not be written down at all.
--
-- 3. Spend could not be attributed to a bead. usage_records carries role, cell
--    and rig from cf-aig-metadata, and no bead — so even with a ceiling there
--    was nothing to compare against it.
--
-- The resolution order is bead, then project, then cell: the most specific
-- budget that exists wins, and spend is summed at that same level. A bead with
-- no budget of its own is therefore governed by its project's, which is the
-- behaviour that makes setting a project budget useful on day one rather than
-- after every bead has been enumerated.

BEGIN;

-- ---------------------------------------------------------------------------
-- A budget that can hold a real number, and name a piece of work
-- ---------------------------------------------------------------------------
ALTER TABLE budget_limits
    ALTER COLUMN daily_cost_cents TYPE numeric(16,4);

ALTER TABLE budget_limits
    ADD COLUMN IF NOT EXISTS work_ref_id uuid REFERENCES work_refs(id) ON DELETE CASCADE;

ALTER TABLE budget_limits DROP CONSTRAINT IF EXISTS budget_has_subject;
ALTER TABLE budget_limits ADD CONSTRAINT budget_has_subject
    CHECK (num_nonnulls(project_id, execution_cell_id, work_ref_id) = 1);

-- One budget per subject. Without this, two rows for the same bead both apply
-- and which one wins is whichever the planner returns first.
CREATE UNIQUE INDEX IF NOT EXISTS budget_limits_project   ON budget_limits (project_id)        WHERE project_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS budget_limits_cell      ON budget_limits (execution_cell_id) WHERE execution_cell_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS budget_limits_work_ref  ON budget_limits (work_ref_id)       WHERE work_ref_id IS NOT NULL;

-- agent_runs.cost_cents has the same integer defect and is empty, so this is the
-- cheapest moment it will ever be fixed.
ALTER TABLE agent_runs
    ALTER COLUMN cost_cents TYPE numeric(16,8);

-- ---------------------------------------------------------------------------
-- Spend that knows which bead it belongs to
-- ---------------------------------------------------------------------------
--
-- cf-aig-metadata allows five keys and carried three (role, cell, rig), so the
-- bead fits. It is written at SLING time rather than at deploy time, because a
-- role is a property of the cell and a bead is a property of one dispatch.
ALTER TABLE usage_records
    ADD COLUMN IF NOT EXISTS bead text;

CREATE INDEX IF NOT EXISTS usage_records_bead
    ON usage_records (bead, occurred_at DESC) WHERE bead IS NOT NULL;

CREATE OR REPLACE FUNCTION system_record_usage(
    p_external_id text,
    p_gateway     text,
    p_provider    text,
    p_model       text,
    p_input       bigint,
    p_output      bigint,
    p_cost_cents  numeric,
    p_cached      boolean,
    p_succeeded   boolean,
    p_role        text,
    p_cell        text,
    p_rig         text,
    p_occurred_at timestamptz,
    p_bead        text DEFAULT NULL
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_project uuid;
    v_matches integer;
    v_new     integer;
BEGIN
    IF p_external_id IS NULL OR p_external_id = '' THEN
        RAISE EXCEPTION 'a usage record needs the gateway log id, or the import cannot be idempotent';
    END IF;

    IF p_cell IS NOT NULL AND p_cell <> '' THEN
        -- Counted first, then read: "more than one match" must be a visible
        -- branch and not a row that happened to come out first (0027).
        SELECT count(*) INTO v_matches
          FROM projects p
          JOIN execution_cells c ON c.id = p.execution_cell_id
         WHERE c.slug = p_cell;

        IF v_matches = 1 THEN
            SELECT p.id INTO v_project
              FROM projects p
              JOIN execution_cells c ON c.id = p.execution_cell_id
             WHERE c.slug = p_cell;
        END IF;
    END IF;

    INSERT INTO usage_records
        (project_id, provider, model, input_tokens, output_tokens, cost_cents,
         occurred_at, gateway, external_id, cached, succeeded, role, cell, rig, bead)
    VALUES
        (v_project, p_provider, p_model, p_input, p_output, p_cost_cents,
         p_occurred_at, p_gateway, p_external_id, p_cached, p_succeeded,
         p_role, p_cell, p_rig, nullif(btrim(coalesce(p_bead, '')), ''))
    ON CONFLICT (external_id) WHERE external_id IS NOT NULL DO NOTHING;

    GET DIAGNOSTICS v_new = ROW_COUNT;
    RETURN v_new > 0;
END
$$;

-- ---------------------------------------------------------------------------
-- Setting a budget
-- ---------------------------------------------------------------------------
--
-- One function for all three subjects rather than three, because the caller
-- always knows which kind it means and the validation is identical. A bead is
-- named by its Beads id, which is what a person types; the work_ref is looked
-- up, and its absence is an error rather than a silently ignored budget.
CREATE OR REPLACE FUNCTION system_set_budget(
    p_subject_kind text,             -- 'bead' | 'project' | 'cell'
    p_subject_key  text,             -- bead id, project slug, or cell slug
    p_daily_cents  numeric,
    p_max_agents   integer DEFAULT 2,
    p_max_runtime_minutes integer DEFAULT NULL
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_project  uuid;
    v_cell     uuid;
    v_work_ref uuid;
    v_changed  integer;
BEGIN
    IF p_daily_cents IS NULL OR p_daily_cents < 0 THEN
        RAISE EXCEPTION 'a daily budget must be zero or more, not %', p_daily_cents;
    END IF;

    CASE p_subject_kind
        WHEN 'bead' THEN
            SELECT id INTO v_work_ref FROM work_refs WHERE bead_id = p_subject_key LIMIT 1;
            IF v_work_ref IS NULL THEN
                RAISE EXCEPTION 'no work reference for bead %; it has not been seen by the platform yet', p_subject_key;
            END IF;
        WHEN 'project' THEN
            SELECT id INTO v_project FROM projects WHERE slug = p_subject_key;
            IF v_project IS NULL THEN RAISE EXCEPTION 'no project named %', p_subject_key; END IF;
        WHEN 'cell' THEN
            SELECT id INTO v_cell FROM execution_cells WHERE slug = p_subject_key;
            IF v_cell IS NULL THEN RAISE EXCEPTION 'no execution cell named %', p_subject_key; END IF;
        ELSE
            RAISE EXCEPTION 'subject kind must be bead, project or cell, not %', p_subject_kind;
    END CASE;

    -- Update first, insert if there was nothing to update.
    --
    -- Not ON CONFLICT: the id defaults to a fresh uuid on every call, so the
    -- primary key never collides. What collides is one of the partial unique
    -- indexes above, and an ON CONFLICT naming the pkey would let that raise
    -- instead of updating — the failure would be "duplicate key value violates
    -- unique constraint budget_limits_work_ref" on the second attempt to set a
    -- budget, which reads like a bug in the caller.
    UPDATE budget_limits
       SET daily_cost_cents      = p_daily_cents,
           max_concurrent_agents = coalesce(p_max_agents, 2),
           max_runtime_minutes   = p_max_runtime_minutes,
           updated_at            = now()
     WHERE (v_work_ref IS NOT NULL AND work_ref_id       = v_work_ref)
        OR (v_project  IS NOT NULL AND project_id        = v_project)
        OR (v_cell     IS NOT NULL AND execution_cell_id = v_cell);
    GET DIAGNOSTICS v_changed = ROW_COUNT;

    IF v_changed = 0 THEN
        INSERT INTO budget_limits
            (project_id, execution_cell_id, work_ref_id,
             daily_cost_cents, max_concurrent_agents, max_runtime_minutes)
        VALUES
            (v_project, v_cell, v_work_ref,
             p_daily_cents, coalesce(p_max_agents, 2), p_max_runtime_minutes);
        RETURN true;
    END IF;

    -- An update that set the same values still reports a row, so this returns
    -- "there is a budget here" rather than "something changed". The caller that
    -- cares about change is Ansible, and nothing sets budgets from Ansible.
    RETURN true;
END
$$;

REVOKE ALL ON FUNCTION system_set_budget(text, text, numeric, integer, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_set_budget(text, text, numeric, integer, integer) TO workgraph_app;

COMMIT;
