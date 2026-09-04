-- What happened to a bead: its outcome, the agent's own words, and what it cost
-- (wg-m07).
--
-- Three beads were dispatched on 4 September. All three reported `done` because
-- wg-runner exited 0. The agent's conclusion was the opposite:
--
--   "I couldn't complete sa-kfh: I searched the entire sandbox environment and
--    found no PortalJS source anywhere... Leaving this open since there's no
--    file to make the rename in."
--
-- It refused to fabricate, left the bead open and wrote down exactly what it
-- looked for. The system then presented that as a completed job, and the
-- explanation existed only in the graph and in a JSONL file on the execution
-- node. Getting `done` wrong in that direction is how people stop trusting the
-- status field.
--
-- wg:backfill -- adds columns and functions; the UPDATE only widens a CHECK.

BEGIN;

-- ---------------------------------------------------------------------------
-- The agent's own words, carried up with the bead
-- ---------------------------------------------------------------------------
--
-- One comment, the most recent. Not a comment table: the graph is canonical for
-- the conversation on a bead, and copying all of it here would be a second
-- store to keep in step (ADR-0027). What is needed is the answer to "did
-- something say why", and the newest comment is that answer.
ALTER TABLE work_refs ADD COLUMN IF NOT EXISTS last_comment text;
ALTER TABLE work_refs ADD COLUMN IF NOT EXISTS last_comment_at timestamptz;
ALTER TABLE work_refs ADD COLUMN IF NOT EXISTS last_comment_by text;

-- ---------------------------------------------------------------------------
-- Projection, now carrying the comment
-- ---------------------------------------------------------------------------
--
-- Both prior signatures dropped, the lesson from 0078: dropping only the older
-- one leaves the newer as a second overload and the next call is ambiguous.
DROP FUNCTION IF EXISTS system_project_bead(text, text, text, text, text);
DROP FUNCTION IF EXISTS system_project_bead(text, text, text, text, text, text[]);

CREATE FUNCTION system_project_bead(
    p_cell text, p_bead text, p_title text, p_kind text, p_status text,
    p_labels text[] DEFAULT NULL,
    p_comment text DEFAULT NULL,
    p_comment_at timestamptz DEFAULT NULL,
    p_comment_by text DEFAULT NULL
) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
DECLARE
    v_cell uuid; v_org uuid; v_db uuid; v_project uuid; v_matches integer;
    v_slug text; v_slugs text[];
BEGIN
    SELECT id INTO v_cell FROM execution_cells WHERE slug = p_cell;
    IF v_cell IS NULL THEN
        RAISE EXCEPTION 'no execution cell named %; register it first', p_cell;
    END IF;

    SELECT id INTO v_org FROM organisations ORDER BY created_at LIMIT 1;

    SELECT id INTO v_db FROM beads_databases
     WHERE execution_cell_id = v_cell AND scope IN ('cell', 'project')
     ORDER BY (scope = 'project') DESC LIMIT 1;
    IF v_db IS NULL THEN
        INSERT INTO beads_databases (organisation_id, execution_cell_id, name, scope)
        VALUES (v_org, v_cell, 'cell-' || p_cell, 'cell')
        RETURNING id INTO v_db;
    END IF;

    IF p_labels IS NOT NULL THEN
        SELECT array_agg(DISTINCT s) INTO v_slugs FROM (
            SELECT substring(l from 12) AS s FROM unnest(p_labels) l
             WHERE l LIKE 'wg-project-%'
            UNION ALL
            SELECT substring(l from 9) AS s FROM unnest(p_labels) l
             WHERE l LIKE 'project:%'
        ) found WHERE s IS NOT NULL AND s <> '';

        v_matches := coalesce(array_length(v_slugs, 1), 0);

        IF v_matches > 1 THEN
            RAISE EXCEPTION 'bead % names % projects (%); a bead belongs to one project',
                p_bead, v_matches, array_to_string(v_slugs, ', ');
        END IF;

        IF v_matches = 1 THEN
            v_slug := v_slugs[1];
            SELECT id INTO v_project FROM projects WHERE slug = v_slug;
            IF v_project IS NULL THEN
                RAISE EXCEPTION 'bead % is labelled for project %, and no project has that slug',
                    p_bead, v_slug;
            END IF;
        END IF;
    END IF;

    IF v_project IS NULL THEN
        SELECT count(*) INTO v_matches FROM projects p WHERE p.execution_cell_id = v_cell;
        IF v_matches = 1 THEN
            SELECT p.id INTO v_project FROM projects p WHERE p.execution_cell_id = v_cell;
        END IF;
    END IF;

    INSERT INTO work_refs (organisation_id, execution_cell_id, beads_database_id,
                           bead_id, title, kind, status, project_id, last_seen_at,
                           last_comment, last_comment_at, last_comment_by)
    VALUES (v_org, v_cell, v_db, p_bead, p_title, p_kind, p_status, v_project, now(),
            p_comment, p_comment_at, p_comment_by)
    ON CONFLICT (organisation_id, execution_cell_id, beads_database_id, bead_id)
    DO UPDATE SET title = EXCLUDED.title,
                  kind = EXCLUDED.kind,
                  status = EXCLUDED.status,
                  project_id = COALESCE(EXCLUDED.project_id, work_refs.project_id),
                  -- COALESCE like project_id: a pass from an older node that
                  -- reports no comment must not erase one an earlier pass
                  -- carried up. A comment is only ever replaced by a comment.
                  last_comment = COALESCE(EXCLUDED.last_comment, work_refs.last_comment),
                  last_comment_at = COALESCE(EXCLUDED.last_comment_at, work_refs.last_comment_at),
                  last_comment_by = COALESCE(EXCLUDED.last_comment_by, work_refs.last_comment_by),
                  last_seen_at = now();
    RETURN true;
END
$$;

REVOKE ALL ON FUNCTION system_project_bead(text, text, text, text, text, text[], text, timestamptz, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_project_bead(text, text, text, text, text, text[], text, timestamptz, text) TO workgraph_app;

-- ---------------------------------------------------------------------------
-- One bead, everything known about it
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION system_bead_detail(p_bead text)
RETURNS jsonb
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    w        record;
    latest   record;
    v_outcome text;
    result   jsonb;
BEGIN
    SELECT r.*, c.slug AS cell_slug, p.slug AS project_slug
      INTO w
      FROM work_refs r
      LEFT JOIN execution_cells c ON c.id = r.execution_cell_id
      LEFT JOIN projects p ON p.id = r.project_id
     WHERE r.bead_id = p_bead
     ORDER BY r.last_seen_at DESC NULLS LAST
     LIMIT 1;

    IF w IS NULL OR w.bead_id IS NULL THEN
        RETURN NULL;
    END IF;

    -- The same predicate as work_refs_read, spelled out rather than relied
    -- upon: this function is SECURITY DEFINER, so the policy does not apply to
    -- it and the check has to be here. A company-scoped bead (no project) is
    -- readable by any authenticated caller, which is what the policy says.
    IF current_app_user() IS NULL THEN
        RAISE EXCEPTION 'a bead detail needs an authenticated caller';
    END IF;
    IF w.project_id IS NOT NULL AND NOT can_read_project(w.project_id) THEN
        -- Indistinguishable from "no such bead", deliberately. Confirming that
        -- a bead exists in a project the caller cannot see is itself a leak
        -- (ADR-0013).
        RETURN NULL;
    END IF;

    -- The most recent run for this bead.
    SELECT * INTO latest
      FROM work_queue
     WHERE bead = p_bead
     ORDER BY created_at DESC
     LIMIT 1;

    -- The outcome, derived rather than stored.
    --
    -- `done` in work_queue means wg-runner exited 0, which is not the same as
    -- the work being finished: on 4 September three runs exited 0, left their
    -- beads open, and reported that they could not proceed. So the honest
    -- outcome combines the two facts we already have -- did the run succeed,
    -- and is the bead still open.
    --
    -- Derived rather than added as a column because both inputs are already
    -- recorded and a stored copy would be a third thing to keep in step.
    v_outcome := CASE
        WHEN latest IS NULL OR latest.id IS NULL THEN 'never_dispatched'
        WHEN latest.status IN ('queued', 'running') THEN latest.status
        WHEN latest.status = 'failed' THEN 'failed'
        -- Ran, exited 0, and the bead is still open: the agent did not finish
        -- the work. Usually it says why in a comment, which is carried above.
        WHEN w.status IS DISTINCT FROM 'closed' THEN 'blocked'
        ELSE 'done'
    END;

    SELECT jsonb_build_object(
        'bead', w.bead_id,
        'title', w.title,
        'kind', w.kind,
        'status', w.status,
        'project', w.project_slug,
        'cell', w.cell_slug,
        'last_seen', w.last_seen_at,

        -- What happened, in one word plus the evidence for it.
        'outcome', v_outcome,
        'run', CASE WHEN latest IS NULL OR latest.id IS NULL THEN NULL ELSE jsonb_build_object(
            'job', latest.id,
            'status', latest.status,
            'rig', latest.rig,
            'created', latest.created_at,
            'claimed', latest.claimed_at,
            'finished', latest.finished_at,
            -- Trimmed: the full runner log is thousands of lines of transcript
            -- and the useful part is the tail, which carries the failure.
            'log_tail', right(coalesce(latest.result, ''), 2000)
        ) END,

        -- The agent's own words, which is the most useful artefact of a run and
        -- was previously reachable only by reading the graph.
        'comment', CASE WHEN w.last_comment IS NULL THEN NULL ELSE jsonb_build_object(
            'text', w.last_comment,
            'at', w.last_comment_at,
            'by', w.last_comment_by
        ) END,

        -- Which model, and what it cost. Per model rather than a single total,
        -- because "which model was used" is a question people ask and a sum
        -- cannot answer it.
        'spend', jsonb_build_object(
            'cents', coalesce((SELECT round(sum(cost_cents), 3) FROM usage_records u
                                WHERE u.bead = p_bead), 0),
            'calls', coalesce((SELECT count(*) FROM usage_records u
                                WHERE u.bead = p_bead), 0),
            'by_model', coalesce((
                SELECT jsonb_agg(m ORDER BY (m->>'cents')::numeric DESC) FROM (
                    SELECT jsonb_build_object(
                        'model', u.model,
                        'provider', u.provider,
                        'calls', count(*),
                        'input_tokens', sum(u.input_tokens),
                        'output_tokens', sum(u.output_tokens),
                        'cents', round(sum(u.cost_cents), 3)
                    ) AS m
                      FROM usage_records u
                     WHERE u.bead = p_bead
                     GROUP BY u.model, u.provider
                ) models), '[]'::jsonb),
            -- Usage arrives from the AI Gateway on an hourly import, so a run
            -- that finished minutes ago shows no spend yet. Saying so beats a
            -- zero that reads as free.
            'newest_record', (SELECT max(occurred_at) FROM usage_records u
                               WHERE u.bead = p_bead)
        )
    ) INTO result;

    RETURN result;
END
$$;

REVOKE ALL ON FUNCTION system_bead_detail(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_bead_detail(text) TO workgraph_app;

COMMIT;
