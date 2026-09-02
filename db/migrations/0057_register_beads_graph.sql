-- Registering a provisioned Beads graph (WP-H4).
--
-- The control plane runs `bd` against paths from beads_databases, and every row
-- there had an empty path: 0035 creates a `cell-<name>` row on first sight of a
-- cell, inferring the graph from the cell rather than from anything
-- provisioned. That is fine for attribution and useless for running a command.
--
-- Now that a graph is provisioned per project, the path is a real thing on a
-- real host and belongs in the registry beside the cells -- fed by the same
-- desired-state document, from Ansible facts, rather than typed twice.
BEGIN;

ALTER TABLE beads_databases ADD COLUMN IF NOT EXISTS host text;

COMMENT ON COLUMN beads_databases.path IS
  'Absolute directory of the graph on its host. Empty for a row inferred from a cell rather than provisioned (WP-H4).';
COMMENT ON COLUMN beads_databases.host IS
  'The host the path is on. A path with no host is not runnable from anywhere in particular.';

-- Register a graph, idempotently on (organisation, name).
--
-- Returns whether anything changed, in the same shape the cell registration
-- uses, because WP-B2's acceptance criterion is that a second Ansible run
-- reports no changes -- which has to be derived from what the function did
-- rather than assumed by the playbook.
CREATE OR REPLACE FUNCTION system_register_beads_graph(
    p_name       text,
    p_path       text,
    p_host       text,
    p_scope      text,
    p_project    text DEFAULT NULL
) RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_org     uuid;
    v_project uuid;
    v_id      uuid;
    v_before  record;
BEGIN
    IF p_name IS NULL OR length(trim(p_name)) = 0 THEN
        RAISE EXCEPTION 'a graph needs a name';
    END IF;
    IF p_path IS NULL OR left(p_path, 1) <> '/' THEN
        -- Relative paths are refused rather than resolved. The command runs
        -- with a working directory nobody chose, and "resolved against
        -- whatever cwd systemd gave us" is not a location.
        RAISE EXCEPTION 'graph % needs an absolute path, got %', p_name, coalesce(p_path, '(null)');
    END IF;

    SELECT id INTO v_org FROM organisations ORDER BY created_at LIMIT 1;

    IF p_project IS NOT NULL THEN
        SELECT id INTO v_project FROM projects WHERE slug = p_project;
        IF v_project IS NULL THEN
            -- Refused rather than registered unattached. A graph whose project
            -- does not exist is a typo, and an unattached graph silently
            -- collects work nobody can attribute.
            RAISE EXCEPTION 'graph % names project %, which is not registered', p_name, p_project;
        END IF;
    END IF;

    SELECT path, host, project_id, scope INTO v_before
      FROM beads_databases WHERE organisation_id = v_org AND name = p_name;

    INSERT INTO beads_databases (organisation_id, name, path, host, scope, project_id)
    VALUES (v_org, p_name, p_path, p_host, p_scope, v_project)
    ON CONFLICT (organisation_id, name) DO UPDATE
        SET path = excluded.path,
            host = excluded.host,
            scope = excluded.scope,
            project_id = excluded.project_id
    RETURNING id INTO v_id;

    IF v_before IS NULL THEN
        RETURN p_name || ' (registered)';
    ELSIF v_before.path IS DISTINCT FROM p_path
       OR v_before.host IS DISTINCT FROM p_host
       OR v_before.project_id IS DISTINCT FROM v_project
       OR v_before.scope IS DISTINCT FROM p_scope THEN
        RETURN p_name || ' (updated)';
    END IF;
    RETURN p_name || ' (unchanged)';
END
$$;

REVOKE ALL ON FUNCTION system_register_beads_graph(text, text, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_register_beads_graph(text, text, text, text, text) TO workgraph_app;

COMMIT;
