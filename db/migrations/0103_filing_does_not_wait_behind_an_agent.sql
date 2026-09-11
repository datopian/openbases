-- A claim can ask for particular kinds of job.
--
-- The dispatcher runs one job at a time and a work job runs for as long as an
-- agent takes. Filing a plan takes about two seconds and starts no agent, but
-- it sat in the same FIFO: on 2026-09-11 a filing queued at 10:14 was still
-- queued at 10:54, behind a run that had been installing dependencies for
-- thirty-seven minutes. The person filing it had re-planned and was waiting on
-- beads that could have existed immediately.
--
-- So the node can now claim by kind, and runs two loops: one for work, which
-- is serialised because agents are expensive and the box is small, and one for
-- filing, which is neither.
--
-- The one-argument form stays, defaulting to "any kind", because a node
-- running the previous binary keeps working through a deploy -- the migration
-- lands before the binaries do.
BEGIN;

-- The one-argument signature is DROPPED, not left alongside.
--
-- CREATE OR REPLACE with an added parameter does not replace anything: the
-- signature differs, so Postgres keeps both, and then a one-argument call --
-- which is what every control-api binary makes until the new one is installed
-- seconds later -- fails with
--
--   function system_claim_work(unknown) is not unique
--
-- and the node claims nothing at all. Probed against staging before shipping,
-- which is the only reason this is not an outage: the same trap is written
-- into 0072 and it was still walked into.
--
-- With only the two-argument form present, a one-argument call resolves to it
-- through the default.
DROP FUNCTION IF EXISTS system_claim_work(text);

CREATE OR REPLACE FUNCTION system_claim_work(p_cell text, p_kinds text[] DEFAULT NULL)
RETURNS TABLE (id uuid, kind text, bead text, brief text, rig text, project text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
DECLARE v_id uuid;
BEGIN
    SELECT q.id INTO v_id
      FROM work_queue q
     WHERE q.cell = p_cell
       AND q.status = 'queued'
       AND (p_kinds IS NULL OR q.kind = ANY (p_kinds))
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

REVOKE ALL ON FUNCTION system_claim_work(text, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_claim_work(text, text[]) TO workgraph_app;

COMMIT;
