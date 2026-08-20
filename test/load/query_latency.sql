-- WP-I3 criterion 2: the UI stays usable at load.
--
-- Measures the queries the interface actually issues, as workgraph_app with a
-- real user set, against 50 projects and 105,000 work items. Timing is reported
-- by \timing rather than computed, so there is nothing to get wrong.
--
-- The specific worry being measured: row-level security on work_refs is
-- can_read_project_row(project_id), a SECURITY DEFINER function. PostgreSQL
-- calls it once per candidate row, so an unfiltered listing is 105,000 function
-- invocations. Whether that matters depends entirely on whether the planner can
-- use an index to reduce the candidate set first.

\set ON_ERROR_STOP on
\timing on

CREATE TEMP TABLE probe AS
SELECT u.id AS user_id, p.id AS project_id
  FROM users u
  JOIN project_memberships m ON m.user_id = u.id
  JOIN projects p ON p.id = m.project_id
 WHERE u.primary_email LIKE 'load-user-%'
 LIMIT 1;
GRANT SELECT ON probe TO workgraph_app;

SET ROLE workgraph_app;
SELECT set_config('workgraph.user_id', (SELECT user_id::text FROM probe), false);

\echo
\echo '=== 1. the open-work list, project-scoped: the most common UI query ==='
SELECT count(*) FROM work_refs
 WHERE project_id = (SELECT project_id FROM probe) AND status = 'open';

\echo
\echo '=== 2. the same, paged as the UI pages it ==='
SELECT bead_id, title, kind FROM work_refs
 WHERE project_id = (SELECT project_id FROM probe) AND status = 'open'
 ORDER BY last_seen_at DESC
 LIMIT 50;

\echo
\echo '=== 3. everything this user can see, unfiltered: the pathological case ==='
SELECT count(*) FROM work_refs;

\echo
\echo '=== 4. the portfolio view: one row per project ==='
SELECT project_id, count(*) FROM work_refs
 WHERE status = 'open'
 GROUP BY project_id;

\echo
\echo '=== 5. does the planner use the index, or call the RLS function per row? ==='
RESET ROLE;
