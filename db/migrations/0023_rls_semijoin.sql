-- Make the row-level security predicate set-based, so the planner stops calling
-- a function once per candidate row (wg-1ng).
--
-- MEASURED on the WP-I3 load fixture, 50 projects and 105,000 work items:
--
--   the unfiltered listing        8779 ms  ->    27 ms
--   the portfolio view             396 ms  ->     5 ms
--   the project-scoped open list    17 ms  ->     6 ms
--
-- WHY the old form was slow. can_read_project tests membership with a
-- CORRELATED subquery — EXISTS (... WHERE m.project_id = p_project_id ...) —
-- which PostgreSQL must re-execute for every row it cannot exclude by index
-- first. Written as an UNCORRELATED set — project_id IN (SELECT project_id FROM
-- project_memberships WHERE user_id = current_app_user()) — the planner builds a
-- hashed subplan once per query and probes it in constant time per row.
--
-- WHY THE OBVIOUS VERSION OF THIS FIX IS 18x WORSE, recorded because it is the
-- thing a reader will try. Changing can_read_project's BODY to use the
-- uncorrelated form takes the unfiltered listing from 8779 ms to 160,486 ms.
-- Inside a function the subquery becomes a SubPlan re-executed per call, and it
-- scans the memberships instead of using the (user_id, project_id) index that
-- 0019 added for the correlated form. The predicate has to reach the PLANNER, so
-- it has to be in the policy. The functions are left exactly as they are, and
-- still used by application code and by the bespoke policies below.
--
-- EQUIVALENCE is not argued from the shape of the SQL. Visibility was captured
-- for every user over every table below, before and after, and compared as id
-- sets rather than counts: two sets of the same size can differ, and that is
-- precisely the failure that would matter.
--
-- The two shapes are NOT interchangeable and are preserved separately:
--
--   can_read_project_row(project_id) requires an authenticated user even for a
--   NULL project_id;
--
--   work_refs's (project_id IS NULL OR can_read_project(project_id)) does not —
--   a company-scoped row is visible with no user set. Collapsing them into one
--   predicate would silently tighten work_refs, which is a behaviour change
--   wearing a performance change's clothes.

BEGIN;

DO $$
DECLARE
    t text;
    -- The set-based form of can_read_project(<col>), exactly: an authenticated
    -- user, a non-null project, and either membership or an unexpired
    -- organisation-wide grant.
    -- The membership test, set-based. Deliberately WITHOUT the inner
    -- "current_app_user() IS NOT NULL AND <col> IS NOT NULL" that
    -- can_read_project carries, because both are redundant here and keeping them
    -- costs 17x: with a NULL user, m.user_id = NULL matches nothing and the
    -- EXISTS is false, so the whole test is false exactly as the function was;
    -- and a NULL <col> cannot match a row of the IN list either.
    --
    -- Measured on the unfiltered listing: 463 ms with the redundant guards
    -- nested inside, 27 ms without. The guards prevent the planner from lifting
    -- the IN into a hashed subplan, which is the entire point of the change.
    can_read constant text := $frag$(
        %1$I IN (
          SELECT m.project_id FROM project_memberships m
           WHERE m.user_id = current_app_user()
        )
        OR EXISTS (
          SELECT 1 FROM role_grants g
           WHERE g.user_id = current_app_user()
             AND g.project_id IS NULL
             AND g.role_name IN ('organisation_admin', 'executive')
             AND (g.expires_at IS NULL OR g.expires_at > now())
        )
      )$frag$;
    row_pred text;
    -- Every table whose policies are exactly can_read_project_row(project_id),
    -- from 0010. Listed rather than discovered so that a table added later is a
    -- deliberate edit here, the same reasoning 0010 gives for its own list.
    --
    -- project_memberships is NOT in this list, and cannot be. The set-based
    -- predicate reads project_memberships, so putting it in that table's own
    -- policy makes the policy reference the table it protects and PostgreSQL
    -- refuses: "infinite recursion detected in policy for relation
    -- project_memberships". The function form survives because the recursion
    -- check does not follow through a function's query boundary.
    --
    -- It keeps can_read_project_row, which is fine: memberships are bounded by
    -- people times projects, so the per-row cost this migration exists to remove
    -- is not material there. Found by running the migration, not by reading it.
    tables text[] := ARRAY[
        'project_repositories', 'beads_databases',
        'agent_profiles', 'agent_runs', 'approval_requests', 'attention_items',
        'context_packs', 'events', 'marketing_signals', 'narrative_snapshots',
        'budget_limits', 'usage_records', 'audit_log'
    ];
BEGIN
    -- can_read_project_row(project_id) is
    --   current_app_user() IS NOT NULL AND (project_id IS NULL OR can_read_project(project_id))
    -- The top-level authentication test IS required here and is not redundant:
    -- can_read_project_row hides even a NULL-project row from a session with no
    -- user, which work_refs's own policy does not.
    row_pred := format(
        '(current_app_user() IS NOT NULL AND (%1$I IS NULL OR %2$s))',
        'project_id', format(can_read, 'project_id'));

    FOREACH t IN ARRAY tables LOOP
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', t || '_read', t);
        EXECUTE format('CREATE POLICY %I ON %I FOR SELECT USING (%s)',
                       t || '_read', t, row_pred);

        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', t || '_update', t);
        EXECUTE format(
            'CREATE POLICY %I ON %I FOR UPDATE USING (%s) WITH CHECK (%s)',
            t || '_update', t, row_pred, row_pred);

        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', t || '_delete', t);
        EXECUTE format('CREATE POLICY %I ON %I FOR DELETE USING (%s)',
                       t || '_delete', t, row_pred);
    END LOOP;

    -- work_refs keeps its own shape: no top-level authentication requirement, so
    -- a company-scoped row stays visible without a user.
    EXECUTE format('DROP POLICY IF EXISTS work_refs_read ON work_refs');
    EXECUTE format('CREATE POLICY work_refs_read ON work_refs FOR SELECT USING (%s)',
                   format('(project_id IS NULL OR %s)', format(can_read, 'project_id')));

    EXECUTE format('DROP POLICY IF EXISTS work_refs_update ON work_refs');
    EXECUTE format('CREATE POLICY work_refs_update ON work_refs FOR UPDATE USING (%s)',
                   format('(project_id IS NULL OR %s)', format(can_read, 'project_id')));

    EXECUTE format('DROP POLICY IF EXISTS work_refs_delete ON work_refs');
    EXECUTE format('CREATE POLICY work_refs_delete ON work_refs FOR DELETE USING (%s)',
                   format('(project_id IS NULL OR %s)', format(can_read, 'project_id')));
END
$$;

-- Left alone deliberately, with the reason, rather than rewritten for symmetry:
--
--   projects, marketing_packets, role_grants, knowledge_records and
--   pull_request_projections have bespoke predicates — a different column, an
--   extra visibility test, or a join through another table. Each is a separate
--   equivalence argument for a table that is bounded in size, so the reward is
--   small and the risk of changing one by pattern-matching is not.
--
-- If any of those grows to six figures, this is the change to repeat for it,
-- one at a time, with the same before-and-after comparison.

COMMIT;
