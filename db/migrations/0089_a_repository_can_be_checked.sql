-- The command that says whether a change works.
--
-- An agent's tools are Read, Grep, Glob, Edit, Write and `Bash(bd:*)`, so it
-- cannot build or test what it writes. It reasons about the code and stops.
-- datopian/portaljs#1662 was a one-line label rename and that was fine; the
-- repository's own CI ran five build jobs on it afterwards and passed. For
-- anything with logic in it, a pull request arrives having never been executed,
-- and the agent cannot see or fix a red build because it never learns of one.
--
-- OPT-IN, per repository, set by a person. Two reasons, and the second is the
-- one that matters:
--
-- A command that is right for one repository is wrong for the next -- ckan is
-- Python, portaljs is a Node monorepo, and there is no single answer.
--
-- And running it executes code FROM THE REPOSITORY. `npm test` runs whatever
-- package.json says, and the agent can edit package.json. So enabling this
-- gives an agent an indirect route to running arbitrary commands as the cell
-- user, which is exactly what the tool policy withholds. That is a decision
-- for a person to take per repository, not a default, and the node narrows it
-- further by running the command with no git credential helper -- so a script
-- cannot mint a token or push, which is the specific thing the deny list
-- exists to prevent.
--
-- NULL means no check runs, which is every repository until somebody sets one.

BEGIN;

ALTER TABLE project_repositories
    ADD COLUMN IF NOT EXISTS check_command text;

ALTER TABLE project_repositories
    DROP CONSTRAINT IF EXISTS project_repositories_check_command_shape;

-- A command or nothing, never whitespace. An empty string would read as
-- "configured" everywhere it is tested for and run as nothing.
ALTER TABLE project_repositories
    ADD CONSTRAINT project_repositories_check_command_shape
    CHECK (check_command IS NULL OR length(btrim(check_command)) > 0);

-- What the node should run in a given rig.
--
-- SECURITY DEFINER and asked by rig, for the same reason system_rig_repository
-- is: the caller is a node, which is a service credential with no app user, so
-- a direct read of an RLS-protected table returns nothing and looks like
-- "no command configured" (0087).
CREATE OR REPLACE FUNCTION system_repository_check(p_cell text, p_rig text)
RETURNS text
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT r.check_command
      FROM execution_rigs e
      JOIN execution_cells c ON c.id = e.execution_cell_id
      JOIN project_repositories r
        ON r.provider = e.provider
       AND lower(r.owner) = lower(e.owner)
       AND lower(r.name) = lower(e.name)
     WHERE c.slug = p_cell
       AND e.rig = p_rig
     LIMIT 1;
$$;

REVOKE ALL ON FUNCTION system_repository_check(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_repository_check(text, text) TO workgraph_app;

COMMIT;
