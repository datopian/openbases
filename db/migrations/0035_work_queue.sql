-- A queue the execution node pulls from, and a place to see the work (WP-D2/E3).
--
-- The control plane cannot reach an execution node. That is deliberate — nodes
-- have no inbound port and reach the control API outward through Access — so
-- "dispatch this bead" cannot be a call. It is a row the node comes and claims.
--
-- The same poll carries bead state the other way. work_refs has existed since
-- 0001 as the projection of Beads into the control plane and nothing has ever
-- filled it, so the UI could show projects and repositories but never the work
-- itself. A node that is already talking to the control plane every few seconds
-- is the cheapest thing to fill it with.

BEGIN;

CREATE TABLE IF NOT EXISTS work_queue (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 'plan' turns a brief into beads; 'work' runs one bead.
    kind         text NOT NULL CHECK (kind IN ('plan', 'work')),
    cell         text NOT NULL,
    rig          text NOT NULL DEFAULT 'sandbox',
    -- The bead for a work job. NULL for a plan job, which has no bead yet —
    -- producing them is the job.
    bead         text,
    -- The brief for a plan job.
    brief        text,
    status       text NOT NULL DEFAULT 'queued'
                 CHECK (status IN ('queued', 'running', 'done', 'failed')),
    requested_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    claimed_at   timestamptz,
    finished_at  timestamptz,
    -- What the agent said, so a person can see why something failed without
    -- opening a terminal on the node.
    result       text,
    CONSTRAINT plan_has_brief CHECK (kind <> 'plan' OR brief IS NOT NULL),
    CONSTRAINT work_has_bead  CHECK (kind <> 'work' OR bead IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS work_queue_claimable
    ON work_queue (cell, created_at) WHERE status = 'queued';

-- Enqueue. Returns the row id so a caller can follow it.
CREATE OR REPLACE FUNCTION system_enqueue_work(
    p_kind text, p_cell text, p_rig text,
    p_bead text, p_brief text, p_user uuid
) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
DECLARE v_id uuid;
BEGIN
    INSERT INTO work_queue (kind, cell, rig, bead, brief, requested_by)
    VALUES (p_kind, p_cell, coalesce(nullif(p_rig, ''), 'sandbox'),
            nullif(p_bead, ''), nullif(p_brief, ''), p_user)
    RETURNING id INTO v_id;
    RETURN v_id;
END
$$;

-- Claim the oldest queued job for a cell, atomically.
--
-- FOR UPDATE SKIP LOCKED so two pollers cannot take the same job. There is one
-- poller per cell today, and a queue whose correctness depends on that staying
-- true is a queue that breaks the first time somebody runs a second one.
CREATE OR REPLACE FUNCTION system_claim_work(p_cell text)
RETURNS TABLE (id uuid, kind text, bead text, brief text, rig text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
DECLARE v_id uuid;
BEGIN
    SELECT q.id INTO v_id
      FROM work_queue q
     WHERE q.cell = p_cell AND q.status = 'queued'
     ORDER BY q.created_at
     LIMIT 1
     FOR UPDATE SKIP LOCKED;

    IF v_id IS NULL THEN RETURN; END IF;

    UPDATE work_queue SET status = 'running', claimed_at = now() WHERE work_queue.id = v_id;

    RETURN QUERY
      SELECT q.id, q.kind, q.bead, q.brief, q.rig FROM work_queue q WHERE q.id = v_id;
END
$$;

CREATE OR REPLACE FUNCTION system_finish_work(p_id uuid, p_ok boolean, p_result text)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
DECLARE n integer;
BEGIN
    UPDATE work_queue
       SET status = CASE WHEN p_ok THEN 'done' ELSE 'failed' END,
           finished_at = now(),
           -- Truncated: this is a summary for a person, not a log store. An
           -- agent's full output belongs in the journal on the node.
           result = left(coalesce(p_result, ''), 4000)
     WHERE id = p_id AND status = 'running';
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n > 0;
END
$$;

-- Project one bead into the control plane.
--
-- Upsert on the natural key work_refs already has. Beads stays canonical; this
-- is a cache so the UI can rank and filter without asking every cell.
CREATE OR REPLACE FUNCTION system_project_bead(
    p_cell text, p_bead text, p_title text, p_kind text, p_status text
) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
DECLARE
    v_cell uuid; v_org uuid; v_db uuid; v_project uuid; v_matches integer;
BEGIN
    SELECT id INTO v_cell FROM execution_cells WHERE slug = p_cell;
    IF v_cell IS NULL THEN
        RAISE EXCEPTION 'no execution cell named %; register it first', p_cell;
    END IF;

    SELECT id INTO v_org FROM organisations ORDER BY created_at LIMIT 1;

    -- The cell's graph, created on first sight rather than requiring a separate
    -- registration step for something the projection can infer.
    SELECT id INTO v_db FROM beads_databases
     WHERE execution_cell_id = v_cell AND scope = 'project' LIMIT 1;
    IF v_db IS NULL THEN
        INSERT INTO beads_databases (organisation_id, execution_cell_id, name, scope)
        VALUES (v_org, v_cell, 'cell-' || p_cell, 'project')
        RETURNING id INTO v_db;
    END IF;

    -- Same rule as cost attribution: resolve only when the cell maps to exactly
    -- one project, and record nothing rather than guessing (0027).
    SELECT count(*) INTO v_matches FROM projects p WHERE p.execution_cell_id = v_cell;
    IF v_matches = 1 THEN
        SELECT p.id INTO v_project FROM projects p WHERE p.execution_cell_id = v_cell;
    END IF;

    INSERT INTO work_refs (organisation_id, execution_cell_id, beads_database_id,
                           bead_id, title, kind, status, project_id, last_seen_at)
    VALUES (v_org, v_cell, v_db, p_bead, p_title, p_kind, p_status, v_project, now())
    ON CONFLICT (organisation_id, execution_cell_id, beads_database_id, bead_id)
    DO UPDATE SET title = EXCLUDED.title,
                  kind = EXCLUDED.kind,
                  status = EXCLUDED.status,
                  project_id = COALESCE(EXCLUDED.project_id, work_refs.project_id),
                  last_seen_at = now();
    RETURN true;
END
$$;

REVOKE ALL ON FUNCTION system_enqueue_work(text, text, text, text, text, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_claim_work(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_finish_work(uuid, boolean, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_project_bead(text, text, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_enqueue_work(text, text, text, text, text, uuid) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_claim_work(text) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_finish_work(uuid, boolean, text) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_project_bead(text, text, text, text, text) TO workgraph_app;

COMMIT;
