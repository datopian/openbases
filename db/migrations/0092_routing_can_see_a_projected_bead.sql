-- Dispatch must be able to tell "not projected" from "cannot see it".
--
-- internal/dispatchroute asks system_rigs_for_bead which rigs can run a bead.
-- That function is SECURITY DEFINER, correctly: routing is the dispatch path's
-- own decision, made with no app user, and it joins only rig and repository
-- names. When it returns nothing, the code then explains WHY -- and it did that
-- with two plain reads of work_refs, which is where this broke.
--
-- work_refs has RLS forced. Its read policy allows a row when project_id IS
-- NULL, or the caller is a member of the project, or the caller holds an
-- organisation-wide admin or executive grant. With no app user none of those
-- hold, so:
--
--   a bead with NO project  -> visible, because of the IS NULL branch
--   a bead WITH a project   -> invisible
--
-- So `SELECT EXISTS (... WHERE bead_id = $1)` answered false for every
-- project-scoped bead, and dispatch reported bead_not_projected: "no bead has
-- been projected from any cell... wait a few seconds and try again". For a bead
-- that was projected, had a project, and whose project had a repository. The
-- advice was to wait, and waiting could never help.
--
-- Observed on staging: eight beads in project msf, all present in work_refs and
-- all reported as not projected. `SET ROLE workgraph_app; SELECT EXISTS(...)`
-- returns false for them and true for a projectless bead, which is the whole
-- bug in two lines.
--
-- It also explains why this was not caught earlier: every bead dispatched
-- during the sandbox work was projectless, so it took the one branch RLS lets
-- through.
BEGIN;

DROP FUNCTION IF EXISTS system_bead_routing(text);

-- Returns what the router needs to explain itself, in one call:
--
--   projected     the bead exists in work_refs at all
--   project       its project's slug, or '' when it belongs to none
--   repositories  how many repositories that project has registered
--
-- One function rather than two reads: the two questions are asked together and
-- answered from the same snapshot, so they cannot disagree about whether the
-- bead exists.
CREATE OR REPLACE FUNCTION system_bead_routing(p_bead text)
RETURNS TABLE (projected boolean, project text, repositories integer)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT
        EXISTS (SELECT 1 FROM work_refs w WHERE w.bead_id = p_bead),
        COALESCE((SELECT p.slug
                    FROM work_refs w
                    JOIN projects p ON p.id = w.project_id
                   WHERE w.bead_id = p_bead
                   LIMIT 1), ''),
        COALESCE((SELECT count(*)::integer
                    FROM work_refs w
                    JOIN project_repositories r ON r.project_id = w.project_id
                   WHERE w.bead_id = p_bead), 0);
$$;

GRANT EXECUTE ON FUNCTION system_bead_routing(text) TO workgraph_app;

COMMIT;
