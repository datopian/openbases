-- Fix the cell lookup in system_record_usage (wg-2a0).
--
-- 0025 joined on execution_cells.name. There is no such column — cells are
-- identified by `slug` — so every attributed import would have failed with
-- "column c.name does not exist".
--
-- It applied cleanly anyway, which is the lesson worth recording: plpgsql does
-- not resolve identifiers in a function body until the function is CALLED, so a
-- migration can create a function that cannot possibly work and report success.
-- The same is true of every SECURITY DEFINER helper in 0020 and 0021. A green
-- migration run is evidence that the SQL parsed, not that it runs; only a test
-- that calls the function is evidence of that, which is what
-- test/acceptance/cost_import.sh now does.
--
-- One correction to 0025's rationale while here. It says a direct INSERT from
-- workgraph_app "writes NOTHING and reports success". That is what an RLS SELECT,
-- UPDATE or DELETE policy does; an INSERT policy is a WITH CHECK, and a row that
-- fails it is REFUSED with "new row violates row-level security policy", loudly.
-- The conclusion is unchanged and if anything stronger: the importer runs with no
-- user, so current_app_user() is NULL, so it cannot insert at all — not silently,
-- but not at all. test/acceptance/cost_import.sh asserts the refusal.
--
-- Appended rather than edited into 0025 because 0025 is already applied and its
-- checksum is recorded. `wg-migrate -verify` exists to notice an applied
-- migration whose file changed underneath it, and quietly editing one to hide a
-- mistake is precisely what it is there to catch.

BEGIN;

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
         WHERE c.slug = p_cell
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

COMMIT;
