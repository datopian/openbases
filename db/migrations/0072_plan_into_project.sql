-- A plan can be filed into a named project (B2).
--
-- system_enqueue_work took a cell and no project, so every plan job and every
-- bead it produced landed with no project at all. That was survivable while
-- one person used it and is not now: a project-less bead is company work
-- anybody identified may read (0071), so planning a client engagement through
-- this path published the brief and its beads company-wide.
--
-- The project is verified against the CALLER's membership rather than trusted
-- from the request. Filing work into a project you are not in is how somebody
-- puts a client's engagement somewhere they cannot be held to it -- or reads
-- one back out, since 0071 makes the queue readable through the bead's project.
BEGIN;

ALTER TABLE work_queue ADD COLUMN project_id uuid REFERENCES projects(id) ON DELETE SET NULL;

COMMENT ON COLUMN work_queue.project_id IS
  'The project this job files its work into, verified against the requester''s membership at enqueue time. NULL is company-wide work (B2).';

CREATE INDEX work_queue_project ON work_queue (project_id, created_at)
    WHERE project_id IS NOT NULL;

-- The six-argument signature is DROPPED, not left alongside.
--
-- CREATE OR REPLACE with an added parameter does not replace anything: the
-- signature is different, so Postgres creates a second overload. Both then
-- match a six-argument call, and it fails --
--
--   function system_enqueue_work(unknown, unknown, unknown, unknown, unknown,
--   uuid) is not unique
--
-- which would have broken `wg-work plan` and every other six-argument caller
-- the moment this deployed. Found by running the test against staging rather
-- than by reading the migration, where the comment claimed the opposite.
--
-- With only the seven-argument form present, a six-argument call resolves to
-- it through the default.
DROP FUNCTION IF EXISTS system_enqueue_work(text, text, text, text, text, uuid);

CREATE FUNCTION system_enqueue_work(
    p_kind text, p_cell text, p_rig text,
    p_bead text, p_brief text, p_user uuid,
    p_project text DEFAULT NULL
) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
DECLARE
    v_id      uuid;
    v_project uuid;
BEGIN
    IF nullif(trim(coalesce(p_project, '')), '') IS NOT NULL THEN
        SELECT id INTO v_project FROM projects WHERE slug = trim(p_project);
        IF v_project IS NULL THEN
            RAISE EXCEPTION 'no project with the slug %', p_project;
        END IF;

        -- Membership of the REQUESTER, not of whoever the caller says. This
        -- function is SECURITY DEFINER, so p_user arrives as a parameter and
        -- current_app_user() may be unset -- the API enqueues outside a user
        -- session. Checking p_user's membership directly is therefore the only
        -- check available here, and the endpoint must keep passing the
        -- authenticated user rather than one from the request body.
        IF NOT EXISTS (
            SELECT 1 FROM project_memberships m
             WHERE m.project_id = v_project AND m.user_id = p_user)
           AND NOT EXISTS (
            SELECT 1 FROM role_grants g
             WHERE g.user_id = p_user
               AND g.project_id IS NULL
               AND g.role_name IN ('organisation_admin', 'executive')
               AND (g.expires_at IS NULL OR g.expires_at > now()))
        THEN
            RAISE EXCEPTION 'the requester is not a member of project %', p_project;
        END IF;
    END IF;

    INSERT INTO work_queue (kind, cell, rig, bead, brief, requested_by, project_id)
    VALUES (p_kind, p_cell, coalesce(nullif(p_rig, ''), 'sandbox'),
            nullif(p_bead, ''), nullif(p_brief, ''), p_user, v_project)
    RETURNING id INTO v_id;
    RETURN v_id;
END
$$;

REVOKE ALL ON FUNCTION system_enqueue_work(text, text, text, text, text, uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_enqueue_work(text, text, text, text, text, uuid, text) TO workgraph_app;

-- The claim carries the project through to the runner, so the planning agent
-- can file beads with the project's label and the dispatcher can stamp
-- work_refs.project_id.
--
-- Dropped and recreated: a new output column changes the return type, which
-- CREATE OR REPLACE refuses -- "cannot change return type of existing
-- function". Safe because the dispatcher polls: a claim that fails during the
-- migration's transaction is retried seconds later, and the transaction holds
-- for milliseconds.
DROP FUNCTION IF EXISTS system_claim_work(text);

CREATE FUNCTION system_claim_work(p_cell text)
RETURNS TABLE (id uuid, kind text, bead text, brief text, rig text, project text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
DECLARE v_id uuid;
BEGIN
    SELECT q.id INTO v_id
      FROM work_queue q
     WHERE q.cell = p_cell AND q.status = 'queued'
     ORDER BY q.created_at
     FOR UPDATE SKIP LOCKED
     LIMIT 1;

    IF v_id IS NULL THEN
        RETURN;
    END IF;

    UPDATE work_queue q
       SET status = 'running', claimed_at = now()
     WHERE q.id = v_id;

    RETURN QUERY
    SELECT q.id, q.kind, q.bead, q.brief, q.rig, p.slug
      FROM work_queue q
      LEFT JOIN projects p ON p.id = q.project_id
     WHERE q.id = v_id;
END
$$;

REVOKE ALL ON FUNCTION system_claim_work(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_claim_work(text) TO workgraph_app;

-- The queue overview gains the project, so the Work page can show it. Same
-- filter as 0071 -- restated because a new output column changes the return
-- type and CREATE OR REPLACE cannot.
DROP FUNCTION IF EXISTS system_queue_overview(text);

CREATE FUNCTION system_queue_overview(p_cell text DEFAULT NULL)
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
    result      text,
    project     text
)
LANGUAGE sql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
    SELECT q.id, q.kind, q.cell, q.rig, q.bead, q.brief, q.status,
           q.created_at, q.finished_at, q.result, p.slug
      FROM work_queue q
      LEFT JOIN projects p ON p.id = q.project_id
     WHERE (p_cell IS NULL OR q.cell = p_cell)
       AND current_app_user() IS NOT NULL
       AND (
         q.requested_by = current_app_user()
         OR is_company_manager()
         -- The job's own project, now that it has one. Checked before the
         -- bead's, because a plan job has a project and no bead: without this
         -- a client plan job would be invisible to the client's team.
         OR (q.project_id IS NOT NULL AND can_read_project(q.project_id))
         OR EXISTS (
              SELECT 1 FROM work_refs w
               WHERE w.bead_id = q.bead
                 AND w.project_id IS NOT NULL
                 AND can_read_project(w.project_id))
       )
     ORDER BY q.created_at DESC
     LIMIT 100;
$$;

REVOKE ALL ON FUNCTION system_queue_overview(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_queue_overview(text) TO workgraph_app;

COMMIT;
