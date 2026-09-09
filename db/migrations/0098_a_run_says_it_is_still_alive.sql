-- A running job says it is still alive, so a long run can be told from a stuck
-- one.
--
-- work_queue already records claimed_at, runtime and model, so "which bead is
-- running, since when, on what model" is answerable. What is not answerable is
-- the question somebody actually asks about a run that has been going for two
-- hours: is it working, or has it hung?
--
-- Between claim and finish the control plane hears nothing. The dispatcher runs
-- wg-runner with CombinedOutput, which blocks until the process exits, so a run
-- that is thinking and a run that is wedged are the same picture -- and the
-- only remedy was to ssh to the node and look.
--
-- agent_runs exists for something adjacent and has never held a row, so this
-- deliberately does not use it: a table with no producer is not a foundation,
-- and inventing one now would leave two half-answers instead of one whole one.
BEGIN;

ALTER TABLE work_queue ADD COLUMN IF NOT EXISTS heartbeat_at timestamptz;
ALTER TABLE work_queue ADD COLUMN IF NOT EXISTS heartbeat_note text;

COMMENT ON COLUMN work_queue.heartbeat_at IS
    'When the node last reported this run alive. Absent means either not '
    'started or an older node that does not report.';
COMMENT ON COLUMN work_queue.heartbeat_note IS
    'What the node observed, in words: output produced so far and how long '
    'since the agent last wrote anything.';

DROP FUNCTION IF EXISTS system_record_heartbeat(uuid, text);
-- Dropped, not replaced: new output columns change the return type and
-- CREATE OR REPLACE cannot do that.
DROP FUNCTION IF EXISTS system_queue_overview(text);

-- Records a heartbeat for a running job.
--
-- Only while running. A heartbeat for a job that has finished would move a
-- timestamp backwards into a completed row and make a finished run look live,
-- which is the one thing this must never do -- a stale heartbeat is what tells
-- somebody a run is stuck.
--
-- Returns whether it recorded, so the node can stop reporting on a job the
-- control plane no longer considers live rather than reporting into nothing.
CREATE OR REPLACE FUNCTION system_record_heartbeat(p_job uuid, p_note text)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE v_rows integer;
BEGIN
    UPDATE work_queue
       SET heartbeat_at = now(),
           heartbeat_note = left(coalesce(p_note, ''), 200)
     WHERE id = p_job AND status = 'running';
    GET DIAGNOSTICS v_rows = ROW_COUNT;
    RETURN v_rows > 0;
END
$$;

REVOKE ALL ON FUNCTION system_record_heartbeat(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_record_heartbeat(uuid, text) TO workgraph_app;

-- The queue overview carries it, so the interface can say what is happening
-- without a second request.
--
-- claimed_at is added at the same time and matters as much: the interface was
-- showing time since the job was QUEUED, which for a job that waited then ran
-- conflates queue wait with run time -- "2h" on a job that queued for 1h58m
-- and has been running two minutes.
--
-- runtime and model were added in 0090 and have never been shown. A run that
-- has been going for an hour is a different decision depending on which model
-- is doing it, and that fact was already recorded.
--
-- The visibility clause is copied verbatim from 0072 rather than reworked:
-- widening who can see the queue is not part of showing what a run is doing,
-- and rewriting a permission expression while adding columns is how one turns
-- into the other by accident.
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
    result      text,
    project     text,
    claimed_at  timestamptz,
    runtime     text,
    model       text,
    heartbeat_at   timestamptz,
    heartbeat_note text
)
LANGUAGE sql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
    SELECT q.id, q.kind, q.cell, q.rig, q.bead, q.brief, q.status,
           q.created_at, q.finished_at, q.result, p.slug,
           q.claimed_at, q.runtime, q.model, q.heartbeat_at, q.heartbeat_note
      FROM work_queue q
      LEFT JOIN projects p ON p.id = q.project_id
     WHERE (p_cell IS NULL OR q.cell = p_cell)
       AND current_app_user() IS NOT NULL
       AND (
         q.requested_by = current_app_user()
         OR is_company_manager()
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
