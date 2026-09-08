-- A cell says whether it may host several projects, so a new project can find
-- one without a deploy.
--
-- The split this fixes: which cells EXIST is infrastructure -- a cell is a Linux
-- user, a home, cgroup limits and a credential profile (ADR-0002), and creating
-- one means touching the machine. Which PROJECT runs in which cell is data: it
-- changes when somebody starts a project, which is a Tuesday afternoon, not a
-- deploy.
--
-- Those two were conflated in infra/ansible/group_vars/all/registry.yml, which
-- carried a project-to-cell table in the repository. Two consequences, both
-- real: client names sat in a public repository as configuration, and assigning
-- a project to a cell required an Ansible run. A project created through the API
-- or MCP got no cell at all -- which is what happened to msf, and is why eight
-- beads could not be dispatched.
--
-- The registry function already existed and already accepted a cell. What was
-- missing was a DEFAULT, so a caller who did not name one produced a project
-- that could never run.
--
-- `shared` is the cell's own property and belongs with the cell: a shared cell
-- may hold many projects, a client cell holds exactly one. Declared in
-- group_vars beside the cell it describes and registered at deploy time, which
-- is where infrastructure belongs.
BEGIN;

ALTER TABLE execution_cells ADD COLUMN IF NOT EXISTS shared boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN execution_cells.shared IS
    'Whether this cell may host more than one project. A shared cell is where '
    'internal and open-source work goes when nobody names a cell; a client '
    'cell is not shared, because its whole purpose is that one engagement '
    'cannot read another (ADR-0002).';

DROP FUNCTION IF EXISTS system_register_execution_cell(text, text, text, text, integer, integer, integer);
DROP FUNCTION IF EXISTS system_register_execution_cell(text, text, text, text, integer, integer, integer, boolean);

-- Same function, one more field. Defaulted so a document written before this
-- migration still registers a cell rather than failing the deploy.
CREATE OR REPLACE FUNCTION system_register_execution_cell(
    p_node_hostname text,
    p_slug          text,
    p_system_username text,
    p_trust_domain  text,
    p_max_concurrent_agents integer,
    p_cpu_quota_percent integer,
    p_memory_limit_mb   integer,
    p_shared boolean DEFAULT false
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_node    uuid;
    v_changed integer;
BEGIN
    SELECT id INTO v_node FROM execution_nodes WHERE hostname = p_node_hostname;
    IF v_node IS NULL THEN
        RAISE EXCEPTION 'no execution node named %; register the node first', p_node_hostname;
    END IF;

    INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain,
                                 max_concurrent_agents, cpu_quota_percent, memory_limit_mb,
                                 shared)
    VALUES (v_node, p_slug, p_system_username, p_trust_domain,
            coalesce(p_max_concurrent_agents, 2), p_cpu_quota_percent, p_memory_limit_mb,
            coalesce(p_shared, false))
    ON CONFLICT (slug) DO UPDATE SET
        execution_node_id     = EXCLUDED.execution_node_id,
        system_username       = EXCLUDED.system_username,
        trust_domain          = EXCLUDED.trust_domain,
        max_concurrent_agents = EXCLUDED.max_concurrent_agents,
        cpu_quota_percent     = EXCLUDED.cpu_quota_percent,
        memory_limit_mb       = EXCLUDED.memory_limit_mb,
        shared                = EXCLUDED.shared
        -- Same reasoning as the node: a deployment must be able to say it
        -- changed nothing. Every column is compared, so a limit re-derived on a
        -- resized node reports a change and an unchanged one does not.
        --
        -- `shared` is in the comparison as well as the SET. Left out of the
        -- comparison, flipping a cell from shared to not -- which moves where
        -- every unnamed project lands -- would report "unchanged" and scroll
        -- past in a deploy log.
        WHERE (execution_cells.execution_node_id, execution_cells.system_username,
               execution_cells.trust_domain, execution_cells.max_concurrent_agents,
               execution_cells.cpu_quota_percent, execution_cells.memory_limit_mb,
               execution_cells.shared)
          IS DISTINCT FROM
              (EXCLUDED.execution_node_id, EXCLUDED.system_username,
               EXCLUDED.trust_domain, EXCLUDED.max_concurrent_agents,
               EXCLUDED.cpu_quota_percent, EXCLUDED.memory_limit_mb,
               coalesce(EXCLUDED.shared, false));

    GET DIAGNOSTICS v_changed = ROW_COUNT;
    RETURN v_changed > 0;
END
$$;

DROP FUNCTION IF EXISTS system_default_cell();

-- Where work goes when nobody said.
--
-- Returns the empty string rather than guessing when there is no shared cell or
-- more than one. A project silently placed in the wrong trust domain is the
-- failure this whole model exists to prevent, so ambiguity is answered by
-- asking rather than by picking -- the caller turns '' into a message naming
-- the cells it could have been.
CREATE OR REPLACE FUNCTION system_default_cell()
RETURNS text
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT coalesce((
        SELECT c.slug
          FROM execution_cells c
         WHERE c.shared
           AND (SELECT count(*) FROM execution_cells s WHERE s.shared) = 1
    ), '');
$$;

GRANT EXECUTE ON FUNCTION system_default_cell() TO workgraph_app;

COMMIT;
