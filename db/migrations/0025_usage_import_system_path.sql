-- The system path for importing gateway usage (wg-2a0).
--
-- usage_records has row-level security enabled with a project-scoped policy
-- (0010), and the importer runs on a timer with no user. A direct INSERT from
-- workgraph_app therefore writes NOTHING and reports success — the same silent
-- no-op that made reconciliation report zero repositories resynced in 0014, and
-- that the monitor's webhook check hit again in 0020. Three times is enough to
-- treat it as the default rather than the exception: anything that runs without
-- a user goes through a narrow SECURITY DEFINER function.
--
-- The functions are deliberately narrow. system_record_usage takes one log entry
-- and returns whether it was new, and system_usage_watermark returns a single
-- timestamp. Neither can read a row back, so the import path cannot become a way
-- to read another project's spend.

BEGIN;

-- Record one gateway log entry. Returns true when it was new.
--
-- ON CONFLICT DO NOTHING on the external id, so a re-run after a partial failure
-- or an overlapping window cannot double-count. That matters more than it looks:
-- the obvious alternative is to import strictly after the last timestamp, which
-- silently drops any entry that arrived out of order.
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
    p_occurred_at timestamptz
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_project uuid;
    v_new     integer;
BEGIN
    IF p_external_id IS NULL OR p_external_id = '' THEN
        RAISE EXCEPTION 'a usage record needs the gateway log id, or the import cannot be idempotent';
    END IF;

    -- Attribution, where the caller tagged the request. A cell maps to the
    -- project whose work it runs; an untagged request stays unattributed rather
    -- than being assigned to a default, because a wrong attribution is harder to
    -- notice than a missing one.
    IF p_cell IS NOT NULL AND p_cell <> '' THEN
        SELECT p.id INTO v_project
          FROM projects p
          JOIN execution_cells c ON c.id = p.execution_cell_id
         WHERE c.name = p_cell
         LIMIT 1;
    END IF;

    INSERT INTO usage_records
        (project_id, provider, model, input_tokens, output_tokens, cost_cents,
         occurred_at, gateway, external_id, cached, succeeded, role, cell, rig)
    VALUES
        (v_project, p_provider, p_model, p_input, p_output, p_cost_cents,
         p_occurred_at, p_gateway, p_external_id, p_cached, p_succeeded,
         p_role, p_cell, p_rig)
    ON CONFLICT (external_id) WHERE external_id IS NOT NULL DO NOTHING;

    GET DIAGNOSTICS v_new = ROW_COUNT;
    RETURN v_new > 0;
END
$$;

-- How far the import has got, per gateway.
--
-- Returned rather than tracked in a separate table: the records themselves are
-- the watermark, so there is no second piece of state to fall out of step with
-- them. NULL means nothing has been imported for that gateway yet.
CREATE OR REPLACE FUNCTION system_usage_watermark(p_gateway text)
RETURNS timestamptz
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT max(occurred_at) FROM usage_records WHERE gateway = p_gateway;
$$;

REVOKE ALL ON FUNCTION system_record_usage(text, text, text, text, bigint, bigint, numeric, boolean, boolean, text, text, text, timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_usage_watermark(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_record_usage(text, text, text, text, bigint, bigint, numeric, boolean, boolean, text, text, text, timestamptz) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_usage_watermark(text) TO workgraph_app;

COMMIT;
