-- Which repository a rig holds, askable by a service caller.
--
-- 0086's pull request endpoint read execution_rigs directly:
--
--     SELECT e.owner, e.name FROM execution_rigs e
--       JOIN execution_cells c ON c.id = e.execution_cell_id
--      WHERE c.slug = $1 AND e.rig = $2
--
-- execution_rigs has RLS enabled and FORCED (0084), and its read policy is
-- `current_app_user() IS NOT NULL`. A service caller is a node, not a person,
-- and has no app user -- so the policy matched nothing and the endpoint
-- answered
--
--     409 rig_holds_no_repository
--     "rig sandbox on cell oss holds no repository"
--
-- for a rig that plainly does hold one:
--
--     sandbox|datopian|workgraph-agent-sandbox
--
-- The first landing pushed its branch correctly and then could not open the
-- pull request. Measured, as the role the API actually uses:
--
--     SET ROLE workgraph_app; SELECT count(*) FROM execution_rigs;  -->  0
--
-- Every other reader of this table goes through a SECURITY DEFINER function,
-- which is why nothing else hit this. The lesson is narrow and worth writing
-- down: a handler that reads an RLS-protected table with its own inline query
-- is a handler that works for people and fails for nodes, and the failure
-- reads as missing data rather than as a permission problem.

BEGIN;

CREATE OR REPLACE FUNCTION system_rig_repository(p_cell text, p_rig text)
RETURNS TABLE (provider text, owner text, name text)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT e.provider, e.owner, e.name
      FROM execution_rigs e
      JOIN execution_cells c ON c.id = e.execution_cell_id
     WHERE c.slug = p_cell
       AND e.rig = p_rig
       -- A rig that holds half a repository holds none: an owner with no name
       -- cannot be joined to project_repositories and would silently match
       -- nothing, which looks identical to a rig that holds nothing. The
       -- schema refuses that combination, and this refuses to report it.
       AND e.owner IS NOT NULL
       AND e.name IS NOT NULL;
$$;

REVOKE ALL ON FUNCTION system_rig_repository(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_rig_repository(text, text) TO workgraph_app;

COMMIT;
