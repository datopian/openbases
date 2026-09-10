-- No new planning jobs.
--
-- A plan job started an agent to decide what the work was, and cost money to
-- do it. Planning now happens wherever the person doing it already thinks --
-- Cowork, Codex, a text file -- and arrives as a plan to be filed, which runs
-- no agent and spends nothing.
--
-- Refused HERE and not only in the API, because the queue is reachable from
-- the node's own tools and from psql, and "the endpoint is gone" is not the
-- same statement as "this cannot be enqueued". A caller that still asks for
-- one is told what to use instead, in the error, where they will read it.
--
-- Old rows keep their kind. The check constraint still allows 'plan' on
-- purpose: history is what tells somebody why a bead exists and what it cost,
-- and rewriting finished jobs to a kind they never had would make the archive
-- lie. Nothing new can be inserted through the only path that inserts.
BEGIN;

CREATE OR REPLACE FUNCTION system_enqueue_work(
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
    IF p_kind = 'plan' THEN
        RAISE EXCEPTION 'planning jobs are gone: plan in your own tool and file '
            'the result with POST /v1/work/beads, workgraph_beads_file or '
            '`wg-work file <plan.json>`, which runs no agent and costs nothing';
    END IF;

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

COMMIT;
