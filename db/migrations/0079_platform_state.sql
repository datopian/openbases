-- What the platform is actually doing (wg-7bh).
--
-- The data existed and nothing could reach it. On staging, at the time this was
-- written: three execution cells, 20,619 agent health events with the newest
-- two minutes old, nine distinct polecats on one rig, thirteen open attention
-- items, and 75 beads attributed to no project. Every one of those numbers
-- required psql on the control node to see.
--
-- "How many cells, how many agents are running, how deep is the queue, is the
-- work graph healthy" are the questions somebody asks when something feels
-- wrong, and the answer being reachable only by the person with a database
-- shell is the same failure as an alert nobody receives.
--
-- One function returning one document, rather than six endpoints. The caller is
-- a page that draws all of it at once, and six round trips would show a
-- half-consistent picture assembled from six moments.

BEGIN;

CREATE OR REPLACE FUNCTION system_platform_state()
RETURNS jsonb
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    result jsonb;
    -- What counts as an agent being "live". Long enough that a slow pass does
    -- not read as a dead agent, short enough that a dead one stops counting
    -- within a coffee break. The witness reports far more often than this.
    live interval := interval '15 minutes';
BEGIN
    -- Management only, and the whole document rather than row by row.
    --
    -- This is deliberately NOT row-level security: the answer is about the
    -- platform rather than about any project, so there is no per-row rule that
    -- would express it. is_company_manager() is the same gate the work
    -- overview uses (0071), so the reach is one decision in one place.
    IF NOT is_company_manager() THEN
        RAISE EXCEPTION 'platform state is company management only';
    END IF;

    SELECT jsonb_build_object(
        'observed_at', now(),

        -- Cells, with what is running on them and what they are allowed to run.
        'cells', COALESCE((
            SELECT jsonb_agg(c ORDER BY c->>'cell')
              FROM (
                SELECT jsonb_build_object(
                    'cell', ec.slug,
                    'trust_domain', ec.trust_domain,
                    'max_concurrent_agents', ec.max_concurrent_agents,
                    'cpu_quota_percent', ec.cpu_quota_percent,
                    'memory_limit_mb', ec.memory_limit_mb,
                    -- Projects on the cell, because more than one is what
                    -- makes bead attribution depend on labels (wg-43n) and is
                    -- worth seeing next to the cell rather than inferring.
                    'projects', (SELECT count(*) FROM projects p
                                  WHERE p.execution_cell_id = ec.id),
                    'agents_live', (
                        SELECT count(DISTINCT h.polecat) FROM agent_health_events h
                         WHERE h.cell = ec.slug AND h.observed_at > now() - live),
                    'agents_ever', (
                        SELECT count(DISTINCT h.polecat) FROM agent_health_events h
                         WHERE h.cell = ec.slug),
                    'rigs', COALESCE((
                        SELECT jsonb_agg(DISTINCT h.rig) FROM agent_health_events h
                         WHERE h.cell = ec.slug AND h.observed_at > now() - live),
                        '[]'::jsonb),
                    'last_report', (
                        SELECT max(h.observed_at) FROM agent_health_events h
                         WHERE h.cell = ec.slug),
                    -- Escalations are how the witness says an agent is in
                    -- trouble, so a count in the last hour is the difference
                    -- between "busy" and "thrashing".
                    'escalations_last_hour', (
                        SELECT count(*) FROM agent_health_events h
                         WHERE h.cell = ec.slug AND h.action = 'escalate'
                           AND h.observed_at > now() - interval '1 hour')
                ) AS c
                  FROM execution_cells ec
              ) cells
        ), '[]'::jsonb),

        -- The queue, by state. Depth is the number somebody wants when work
        -- appears to be going nowhere.
        'queue', COALESCE((
            SELECT jsonb_object_agg(status, n)
              FROM (SELECT status, count(*) AS n FROM work_queue GROUP BY status) q
        ), '{}'::jsonb),

        -- The work graphs, and how much is projected from each. A graph with
        -- rows is being read; a graph with none either is new or has stopped
        -- being projected, and those look identical without the count.
        'graphs', COALESCE((
            SELECT jsonb_agg(g ORDER BY g->>'name')
              FROM (
                SELECT jsonb_build_object(
                    'name', b.name,
                    'scope', b.scope,
                    'project', p.slug,
                    'cell', ec.slug,
                    'beads', (SELECT count(*) FROM work_refs w
                               WHERE w.beads_database_id = b.id),
                    'last_seen', (SELECT max(w.last_seen_at) FROM work_refs w
                                   WHERE w.beads_database_id = b.id)
                ) AS g
                  FROM beads_databases b
                  LEFT JOIN projects p ON p.id = b.project_id
                  LEFT JOIN execution_cells ec ON ec.id = b.execution_cell_id
              ) graphs
        ), '[]'::jsonb),

        -- Beads belonging to no project, which is the failure wg-43n fixed the
        -- mechanism for and this makes visible. Silence is what made it last.
        'unattributed', COALESCE((
            SELECT jsonb_agg(jsonb_build_object(
                       'cell', u.cell, 'graph', u.graph,
                       'projects_on_cell', u.projects_on_cell, 'beads', u.beads))
              FROM system_unattributed_work() u
        ), '[]'::jsonb),

        -- Open attention items by rule. The alerting half already works and
        -- reaches the inbox; this is the count, so a page can say "thirteen"
        -- without anybody opening thirteen items.
        'alerts', COALESCE((
            SELECT jsonb_agg(jsonb_build_object('rule', a.rule, 'count', a.n,
                                                'newest', a.newest)
                             ORDER BY a.n DESC)
              FROM (SELECT rule, count(*) AS n, max(last_seen_at) AS newest
                      FROM attention_items
                     WHERE resolved_at IS NULL
                     GROUP BY rule) a
        ), '[]'::jsonb),

        'projects', (SELECT count(*) FROM projects),
        'repositories', (SELECT count(*) FROM project_repositories),
        'beads', (SELECT count(*) FROM work_refs)
    ) INTO result;

    RETURN result;
END
$$;

REVOKE ALL ON FUNCTION system_platform_state() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_platform_state() TO workgraph_app;

COMMIT;
