-- Routing can name a rig that does not exist yet.
--
-- The last link in "create a project, attach a repository, dispatch work". The
-- first two are data and happen in seconds. The third needed a deploy, because
-- a rig -- one repository checked out inside a cell's Gas Town -- was only ever
-- created by the provisioner that Ansible runs. Attach a repository on Tuesday
-- and the work could not run until somebody deployed.
--
-- Provisioning on attach was considered and rejected, for a good reason
-- recorded in the execution_cell role: nothing should "start cloning the moment
-- a repository was attached from a phone". Cloning a dozen repositories is a
-- several-minute job and does not belong in an attach call.
--
-- Dispatch time is the answer that respects both. Work has been asked for, so a
-- clone is warranted, and it is one repository rather than all of them. But
-- routing refused before a job could ever be created: system_rigs_for_bead
-- joins execution_rigs, which is what the node HOLDS, so a repository with no
-- rig yet produced no candidate and the refusal "no rig in cell oss holds a
-- repository belonging to msf".
--
-- This is the other half of that question: not what the cell holds, but what it
-- SHOULD hold for this bead's project. Routing consults it when nothing is held,
-- and the dispatcher creates the rig when the job arrives.
BEGIN;

DROP FUNCTION IF EXISTS system_rigs_wanted_for_bead(text, text);

-- The rigs a cell should have for one bead's project, held or not.
--
-- Deliberately built on system_rigs_wanted rather than repeating its logic. The
-- rig name and prefix are derived there -- lower-cased, non-alphanumerics
-- folded, and a hashed suffix so the prefix is stable -- and a second copy of
-- that derivation would eventually disagree about what a rig is called, which
-- means the dispatcher would create `autoclaw_sh` while routing sent work to
-- `autoclaw-sh`.
CREATE OR REPLACE FUNCTION system_rigs_wanted_for_bead(p_bead text, p_cell text)
RETURNS TABLE (
    rig        text,
    repository text,
    clone_url  text,
    prefix     text,
    held       boolean
)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT w.rig, w.owner || '/' || w.name, w.clone_url, w.prefix, w.held
      FROM system_rigs_wanted(p_cell) w
     WHERE w.project = (
             SELECT p.slug
               FROM work_refs r
               JOIN projects p ON p.id = r.project_id
              WHERE r.bead_id = p_bead
              LIMIT 1)
     ORDER BY w.rig;
$$;

REVOKE ALL ON FUNCTION system_rigs_wanted_for_bead(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_rigs_wanted_for_bead(text, text) TO workgraph_app;

COMMIT;
