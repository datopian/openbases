-- Index work_refs by project, which the schema has never had (WP-I3).
--
-- Row-level security on work_refs is can_read_project_row(project_id), a
-- SECURITY DEFINER function PostgreSQL calls once per CANDIDATE row. Whether
-- that is cheap or ruinous depends entirely on whether the planner can shrink
-- the candidate set with an index before evaluating it.
--
-- Measured on the WP-I3 load fixture — 50 projects, 105,000 work items:
--
--   project-scoped open list, with this index      19.9 ms
--   the same query without it                      sequential scan of 105,000
--                                                  rows; the leak test, which
--                                                  asks the question sixty
--                                                  times, took over ten minutes
--
-- Fine at the three pilot projects, which is why it was never noticed. Not fine
-- at fifty, and the go-live target is fifty.
--
-- (project_id, status) rather than project_id alone because every real query
-- pairs them: the interface lists a project's OPEN work, and the portfolio view
-- counts open items per project.

BEGIN;

CREATE INDEX IF NOT EXISTS work_refs_project_status
    ON work_refs (project_id, status);

-- The membership lookup the RLS predicate performs, on the column it filters
-- by. Without it, can_read_project() scans project_memberships on every call —
-- and it is called once per candidate row, so the cost multiplies.
CREATE INDEX IF NOT EXISTS project_memberships_user_project
    ON project_memberships (user_id, project_id);

COMMIT;
