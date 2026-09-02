-- The work overview showed every project's work to everybody (B1).
--
-- system_work_overview and system_queue_overview are SECURITY DEFINER, which
-- is correct -- they join work_refs and usage_records, which are
-- RLS-protected, against work_queue, which is platform state with no project
-- of its own -- and neither filtered by anything. 0036's own comment says
-- "keeping the join behind one SECURITY DEFINER function means the visibility
-- rule is written once, here", and then no rule was written.
--
-- So GET /v1/work returned every bead in every project to any authenticated
-- caller, and the API made it worse by querying outside authz.WithUser: no
-- identity was set, so there was nothing for a filter to have used even if one
-- had existed. CDT and NGED are restricted client projects. Tomorrow four more
-- colleagues can log in (B3).
--
-- Superseding 0036 rather than editing it: migrations are immutable.
BEGIN;

-- The company-wide management test, named once.
--
-- This is its sixth copy -- 0008, 0011, 0023, 0069, 0070 and here -- and the
-- sixth is where it stops being an accident. Named because these two functions
-- need it for rows that have no project at all, where can_read_project has
-- nothing to test.
--
-- STABLE, not IMMUTABLE: it reads role_grants and now(). SECURITY DEFINER so
-- it works from inside a definer function whose caller cannot read
-- role_grants -- which is most callers, since role_grants is behind RLS.
CREATE OR REPLACE FUNCTION is_company_manager() RETURNS boolean
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = public, pg_temp
AS $$
    SELECT EXISTS (
      SELECT 1 FROM role_grants g
       WHERE g.user_id = current_app_user()
         AND g.project_id IS NULL
         AND g.role_name IN ('organisation_admin', 'executive')
         AND (g.expires_at IS NULL OR g.expires_at > now()));
$$;

COMMENT ON FUNCTION is_company_manager() IS
  'Whether the current app user holds an unexpired company-wide organisation_admin or executive grant. The readers of anything that has no project to scope it to (mem-003b900b).';

REVOKE ALL ON FUNCTION is_company_manager() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION is_company_manager() TO workgraph_app;

-- A bead is visible when its project is, and a bead with no project is
-- company-wide work that anybody identified may see. That is the same shape as
-- work_refs' own policy, deliberately: this function exists to join across
-- work_refs, and inventing a second answer to "may this person see this bead"
-- is how the two come to disagree.
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
           (SELECT q.status FROM work_queue q
             WHERE q.bead = w.bead_id ORDER BY q.created_at DESC LIMIT 1),
           (SELECT q.created_at FROM work_queue q
             WHERE q.bead = w.bead_id ORDER BY q.created_at DESC LIMIT 1),
           coalesce((SELECT sum(u.cost_cents) FROM usage_records u WHERE u.bead = w.bead_id), 0),
           coalesce((SELECT count(*) FROM usage_records u WHERE u.bead = w.bead_id), 0)
      FROM work_refs w
      LEFT JOIN execution_cells c ON c.id = w.execution_cell_id
      LEFT JOIN projects p        ON p.id = w.project_id
     WHERE (p_cell IS NULL OR c.slug = p_cell)
       -- Nothing without an identity. A definer function that returns rows to
       -- a caller with no user is the hole this migration closes, and
       -- returning nothing is the only safe answer to "who is asking?" being
       -- unanswered.
       AND current_app_user() IS NOT NULL
       AND (w.project_id IS NULL OR can_read_project(w.project_id))
     ORDER BY w.last_seen_at DESC;
$$;

-- The queue, including plan jobs, which have no bead to hang off.
--
-- A work job is filtered through its bead's project. A plan job has neither --
-- producing beads is the job -- so it is visible to whoever requested it and
-- to company management, and to nobody else. A brief is the text somebody
-- pasted in, and for a client engagement that text is the engagement.
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
     WHERE (p_cell IS NULL OR q.cell = p_cell)
       AND current_app_user() IS NOT NULL
       AND (
         -- Whoever asked for it, whatever it is.
         q.requested_by = current_app_user()
         OR is_company_manager()
         -- Otherwise: a work job through its bead's project. EXISTS rather
         -- than a join, because a queue row whose bead has not been projected
         -- into work_refs yet must not vanish for its requester -- that is
         -- covered by the branch above -- nor appear for anybody else.
         OR EXISTS (
              SELECT 1 FROM work_refs w
               WHERE w.bead_id = q.bead
                 AND w.project_id IS NOT NULL
                 AND can_read_project(w.project_id))
       )
     ORDER BY q.created_at DESC
     LIMIT 100;
$$;

REVOKE ALL ON FUNCTION system_work_overview(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_queue_overview(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_work_overview(text) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_queue_overview(text) TO workgraph_app;

COMMIT;
