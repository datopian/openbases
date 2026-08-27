-- One answer for "what work exists and how is it going" (WP-F1).
--
-- The UI could show projects and repositories but never the work itself,
-- because work_refs was never filled. Now that the dispatcher projects beads
-- into it, this joins the three things a person actually wants together: the
-- bead, whether an agent is queued or running on it, and what it has cost.
--
-- A function rather than a view, for the reason 0031 spells out: work_refs and
-- usage_records are RLS-protected, the API asks with a user, but the queue is
-- platform state with no project of its own. Keeping the join behind one
-- SECURITY DEFINER function means the visibility rule is written once, here,
-- rather than reimplemented by every caller.

BEGIN;

CREATE OR REPLACE FUNCTION system_work_overview(p_cell text DEFAULT NULL)
RETURNS TABLE (
    bead        text,
    title       text,
    kind        text,
    status      text,
    cell        text,
    project     text,
    last_seen   timestamptz,
    queue_state text,
    queued_at   timestamptz,
    spent_cents numeric,
    requests    bigint
)
LANGUAGE sql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
    SELECT w.bead_id,
           w.title,
           w.kind,
           w.status,
           c.slug,
           p.slug,
           w.last_seen_at,
           -- The most recent queue entry for this bead, if any. A bead worked
           -- twice shows its latest run rather than its first.
           (SELECT q.status FROM work_queue q
             WHERE q.bead = w.bead_id ORDER BY q.created_at DESC LIMIT 1),
           (SELECT q.created_at FROM work_queue q
             WHERE q.bead = w.bead_id ORDER BY q.created_at DESC LIMIT 1),
           -- All of it, not just today. A budget is a daily question; "what did
           -- this piece of work cost" is not.
           coalesce((SELECT sum(u.cost_cents) FROM usage_records u WHERE u.bead = w.bead_id), 0),
           coalesce((SELECT count(*) FROM usage_records u WHERE u.bead = w.bead_id), 0)
      FROM work_refs w
      LEFT JOIN execution_cells c ON c.id = w.execution_cell_id
      LEFT JOIN projects p        ON p.id = w.project_id
     WHERE p_cell IS NULL OR c.slug = p_cell
     ORDER BY w.last_seen_at DESC;
$$;

-- The queue itself, including plan jobs, which have no bead to hang off.
CREATE OR REPLACE FUNCTION system_queue_overview(p_cell text DEFAULT NULL)
RETURNS TABLE (
    id          uuid,
    kind        text,
    cell        text,
    rig         text,
    bead        text,
    brief       text,
    status      text,
    created_at  timestamptz,
    finished_at timestamptz,
    result      text
)
LANGUAGE sql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
    SELECT q.id, q.kind, q.cell, q.rig, q.bead, q.brief, q.status,
           q.created_at, q.finished_at, q.result
      FROM work_queue q
     WHERE p_cell IS NULL OR q.cell = p_cell
     ORDER BY q.created_at DESC
     LIMIT 100;
$$;

REVOKE ALL ON FUNCTION system_work_overview(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_queue_overview(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_work_overview(text) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_queue_overview(text) TO workgraph_app;

COMMIT;
