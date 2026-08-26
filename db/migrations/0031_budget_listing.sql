-- Listing budgets needs the system path too (wg-qw1).
--
-- wg-budget's `list` read budget_limits directly. budget_limits is
-- RLS-protected with a project-scoped SELECT policy, and the tool runs on a
-- timer-style unit with no user — so the read returned nothing while the write
-- beside it had succeeded. `wg-budget set cell oss 10000` printed success and
-- `wg-budget list` printed "no budgets are set", both truthfully from where they
-- were standing.
--
-- That is the FIFTH time this pattern has bitten: reconciliation reported zero
-- repositories resynced (0014), the monitor reported zero unprocessed webhooks
-- (0020), the usage import would have written nothing (0025), and the alerting
-- functions before them. The rule is now unambiguous — anything that runs
-- without a user reads through a SECURITY DEFINER function, including the parts
-- that only display things.
--
-- It returns a flattened view rather than the table, so it cannot become a way
-- to read a project id and correlate it elsewhere.

BEGIN;

CREATE OR REPLACE FUNCTION system_list_budgets()
RETURNS TABLE (
    subject_kind text,
    subject_key  text,
    daily_cents  numeric,
    max_agents   integer,
    max_runtime_minutes integer,
    updated_at   timestamptz
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT CASE
             WHEN b.work_ref_id IS NOT NULL THEN 'bead'
             WHEN b.project_id  IS NOT NULL THEN 'project'
             ELSE 'cell'
           END,
           coalesce(w.bead_id, p.slug, c.slug),
           b.daily_cost_cents,
           b.max_concurrent_agents,
           b.max_runtime_minutes,
           b.updated_at
      FROM budget_limits b
      LEFT JOIN work_refs       w ON w.id = b.work_ref_id
      LEFT JOIN projects        p ON p.id = b.project_id
      LEFT JOIN execution_cells c ON c.id = b.execution_cell_id
     ORDER BY 1, 2;
$$;

REVOKE ALL ON FUNCTION system_list_budgets() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_list_budgets() TO workgraph_app;

COMMIT;
