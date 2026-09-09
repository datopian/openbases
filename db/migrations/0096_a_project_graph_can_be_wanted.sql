-- A graph's bead prefix becomes data, so a project can have a graph without a
-- deploy and without losing the prefix its ids depend on.
--
-- The same split as cells and rigs, applied to the last place that got it
-- wrong. infra/ansible/group_vars/control.yml declared every Beads graph,
-- including one per client engagement, each naming the client and its
-- hand-picked prefix. Client names were configuration in a repository that is
-- now public, and a project's first accepted candidate was blocked on "no graph
-- for project X" until somebody ran Ansible.
--
-- Removing that declaration is what makes the prefix column necessary, and this
-- is the part worth reading. The prefix appears in every bead id in a graph and
-- a bead id is NEVER rewritten. Today it is recorded in exactly two places:
-- group_vars, and the graph's own config on disk. Delete the first and rebuild
-- the node, and the on-demand path would create project-cdt with a DERIVED
-- prefix -- cdt7 rather than the cdt every existing id in it carries. So the
-- prefix moves into the database, which is the only copy that survives both a
-- rebuild and the removal of the declaration.
--
-- Set once and never changed. A registration that omits the prefix keeps the
-- recorded one, and one that contradicts it is refused: a graph whose prefix
-- moved is a graph whose ids are all wrong, and that is not something to
-- discover from a deploy log.
BEGIN;

ALTER TABLE beads_databases ADD COLUMN IF NOT EXISTS prefix text;

COMMENT ON COLUMN beads_databases.prefix IS
    'The bead id prefix this graph was initialised with. Immutable once set: '
    'it appears in every id in the graph and an id is never rewritten.';

DROP FUNCTION IF EXISTS system_register_beads_graph(text, text, text, text, text);
DROP FUNCTION IF EXISTS system_register_beads_graph(text, text, text, text, text, text);

CREATE OR REPLACE FUNCTION system_register_beads_graph(
    p_name       text,
    p_path       text,
    p_host       text,
    p_scope      text,
    p_project    text DEFAULT NULL,
    p_prefix     text DEFAULT NULL
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

    SELECT path, host, project_id, scope, prefix INTO v_before
      FROM beads_databases WHERE organisation_id = v_org AND name = p_name;

    -- The one thing that must never move.
    IF v_before.prefix IS NOT NULL
       AND p_prefix IS NOT NULL
       AND v_before.prefix <> p_prefix THEN
        RAISE EXCEPTION 'graph % is prefixed % and cannot be re-registered as %; '
            'every bead id in it carries the old prefix and ids are never rewritten',
            p_name, v_before.prefix, p_prefix;
    END IF;

    INSERT INTO beads_databases (organisation_id, name, path, host, scope, project_id, prefix)
    VALUES (v_org, p_name, p_path, p_host, p_scope, v_project, p_prefix)
    ON CONFLICT (organisation_id, name) DO UPDATE
        SET path = excluded.path,
            host = excluded.host,
            scope = excluded.scope,
            project_id = excluded.project_id,
            -- Recorded when it was not known, kept otherwise. A caller that
            -- does not send one -- an older deploy document, say -- must not
            -- erase it.
            prefix = coalesce(excluded.prefix, beads_databases.prefix)
    RETURNING id INTO v_id;

    IF v_before IS NULL THEN
        RETURN p_name || ' (registered)';
    ELSIF v_before.path IS DISTINCT FROM p_path
       OR v_before.host IS DISTINCT FROM p_host
       OR v_before.project_id IS DISTINCT FROM v_project
       OR v_before.scope IS DISTINCT FROM p_scope
       OR (v_before.prefix IS NULL AND p_prefix IS NOT NULL) THEN
        RETURN p_name || ' (updated)';
    END IF;
    RETURN p_name || ' (unchanged)';
END
$$;

REVOKE ALL ON FUNCTION system_register_beads_graph(text, text, text, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_register_beads_graph(text, text, text, text, text, text) TO workgraph_app;

DROP FUNCTION IF EXISTS system_graph_wanted_for_project(text);

-- The graph a project should have, held or not.
--
-- `held` is what tells the publisher whether to create one. A held graph
-- reports its RECORDED prefix, so an existing graph is never re-prefixed. Only
-- a graph that does not exist gets a derived one -- three characters of the
-- slug plus one of its hash, the same shape as a rig prefix, hashed rather than
-- numbered because a numbered suffix moves when something is inserted before it
-- and an id that moves is an id that is wrong for ever.
CREATE OR REPLACE FUNCTION system_graph_wanted_for_project(p_project text)
RETURNS TABLE (name text, prefix text, host text, held boolean)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT
        coalesce(b.name, 'project-' || p.slug),
        coalesce(
            b.prefix,
            substring(regexp_replace(lower(p.slug), '[^a-z0-9]+', '', 'g') from 1 for 3)
              || substr(md5(p.slug), 1, 1)),
        b.host,
        b.id IS NOT NULL
      FROM projects p
      LEFT JOIN beads_databases b
             ON b.project_id = p.id AND b.scope = 'project'
     WHERE p.slug = p_project
     LIMIT 1;
$$;

REVOKE ALL ON FUNCTION system_graph_wanted_for_project(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_graph_wanted_for_project(text) TO workgraph_app;

COMMIT;
