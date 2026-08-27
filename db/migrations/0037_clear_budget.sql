-- Removing a budget.
--
-- system_set_budget could create one and change one, and nothing could take one
-- away. That is not a missing convenience: budgets resolve most-specific-first,
-- so a budget of zero cents on a bead refuses it forever and there was no way
-- back. The only remedy was raising the number, which leaves a limit in place
-- that nobody meant to set and that hides the project budget underneath.
--
-- Deleting the row restores the fallback rather than approximating it. A bead
-- with no budget of its own is governed by its project, and a project with none
-- by its cell — which is the behaviour someone expects from "unset".
--
-- SECURITY DEFINER for the same reason as its sibling: budget_limits is behind
-- RLS and the callers here are operators at a CLI, whose reads would otherwise
-- come back empty while their writes appeared to succeed.
BEGIN;

CREATE OR REPLACE FUNCTION system_clear_budget(
    p_subject_kind text,             -- 'bead' | 'project' | 'cell'
    p_subject_key  text              -- bead id, project slug, or cell slug
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
    -- The subject is resolved the same way as in system_set_budget, and the
    -- same unknown-subject errors are raised. Clearing a budget for a cell that
    -- does not exist is a typo, and reporting "nothing to clear" would let the
    -- typo pass as success.
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

    DELETE FROM budget_limits
     WHERE (v_work_ref IS NOT NULL AND work_ref_id       = v_work_ref)
        OR (v_project  IS NOT NULL AND project_id        = v_project)
        OR (v_cell     IS NOT NULL AND execution_cell_id = v_cell);
    GET DIAGNOSTICS v_changed = ROW_COUNT;

    -- False means there was nothing there, which the CLI reports plainly. It is
    -- not an error: clearing a budget that is already absent leaves the caller
    -- where they wanted to be.
    RETURN v_changed > 0;
END
$$;

REVOKE ALL ON FUNCTION system_clear_budget(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_clear_budget(text, text) TO workgraph_app;

COMMIT;
