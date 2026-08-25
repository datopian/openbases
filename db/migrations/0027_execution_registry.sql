-- Register execution nodes and cells, and stop guessing which project a cell
-- belongs to (wg-38w).
--
-- execution_nodes and execution_cells were both EMPTY on staging while the oss
-- cell was running, had a Gas Town, and had produced 9,200 agent_health_events.
-- Nothing has ever written them: Ansible knows the nodes (inventory) and the
-- cells (group_vars/execution.yml), and the facts simply were not recorded.
--
-- Three things follow from that, all of them silent:
--
--   Cost attribution cannot resolve. system_record_usage maps a cf-aig-metadata
--   cell tag to a project through execution_cells.slug, so with no cell rows
--   every tagged request stays unattributed however well the tagging works.
--
--   agent_health_events.cell is text with no referent, so "which cell" cannot be
--   joined to anything.
--
--   0009 created nged as confidential rather than restricted, deliberately and
--   with a note: the schema refuses a restricted project with no
--   execution_cell_id, and cells did not exist yet. Tightening it was left for
--   when they did. This is when.
--
-- The functions are SECURITY DEFINER for consistency with every other path that
-- runs without a user, and narrow enough that none of them can read a row back.

BEGIN;

-- Register a node. Idempotent on hostname, which is its natural key.
--
-- The environment is updated on conflict rather than ignored: a node that moved
-- between environments is a fact worth recording, and silently keeping the old
-- value would make the registry disagree with the inventory.
CREATE OR REPLACE FUNCTION system_register_execution_node(
    p_hostname    text,
    p_environment text
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_changed integer;
BEGIN
    IF coalesce(p_hostname, '') = '' THEN
        RAISE EXCEPTION 'an execution node needs a hostname';
    END IF;

    -- Returns whether anything actually changed, not the id.
    --
    -- The caller is a deployment, and the acceptance criterion for the
    -- deployment is that a second run reports no changes (WP-B2). A plain
    -- ON CONFLICT DO UPDATE always reports one row affected, so it would report
    -- a change on every run forever and quietly break that check. The WHERE on
    -- the DO UPDATE is what makes "nothing to do" distinguishable from "done".
    INSERT INTO execution_nodes (hostname, environment)
    VALUES (p_hostname, p_environment)
    ON CONFLICT (hostname) DO UPDATE SET environment = EXCLUDED.environment
        WHERE execution_nodes.environment IS DISTINCT FROM EXCLUDED.environment;

    GET DIAGNOSTICS v_changed = ROW_COUNT;
    RETURN v_changed > 0;
END
$$;

-- Register a cell on a node. Idempotent on slug.
--
-- The limits are recorded as APPLIED rather than as declared. The execution_cell
-- role derives them from the node's real CPU and memory (WP-I3) because the
-- declared numbers were above the hardware and could never be reached; storing
-- the declared ones here would put that same fiction back in the database.
CREATE OR REPLACE FUNCTION system_register_execution_cell(
    p_node_hostname text,
    p_slug          text,
    p_system_username text,
    p_trust_domain  text,
    p_max_concurrent_agents integer,
    p_cpu_quota_percent integer,
    p_memory_limit_mb   integer
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
        -- Refuse rather than create the node implicitly. A cell arriving for an
        -- unknown node means the caller registered them out of order, and
        -- inventing a node with no environment would hide that.
        RAISE EXCEPTION 'no execution node named %; register the node first', p_node_hostname;
    END IF;

    INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain,
                                 max_concurrent_agents, cpu_quota_percent, memory_limit_mb)
    VALUES (v_node, p_slug, p_system_username, p_trust_domain,
            coalesce(p_max_concurrent_agents, 2), p_cpu_quota_percent, p_memory_limit_mb)
    ON CONFLICT (slug) DO UPDATE SET
        execution_node_id     = EXCLUDED.execution_node_id,
        system_username       = EXCLUDED.system_username,
        trust_domain          = EXCLUDED.trust_domain,
        max_concurrent_agents = EXCLUDED.max_concurrent_agents,
        cpu_quota_percent     = EXCLUDED.cpu_quota_percent,
        memory_limit_mb       = EXCLUDED.memory_limit_mb
        -- Same reasoning as the node: a deployment must be able to say it
        -- changed nothing. Every column is compared, so a limit re-derived on a
        -- resized node reports a change and an unchanged one does not.
        WHERE (execution_cells.execution_node_id, execution_cells.system_username,
               execution_cells.trust_domain, execution_cells.max_concurrent_agents,
               execution_cells.cpu_quota_percent, execution_cells.memory_limit_mb)
          IS DISTINCT FROM
              (EXCLUDED.execution_node_id, EXCLUDED.system_username,
               EXCLUDED.trust_domain, EXCLUDED.max_concurrent_agents,
               EXCLUDED.cpu_quota_percent, EXCLUDED.memory_limit_mb);

    GET DIAGNOSTICS v_changed = ROW_COUNT;
    RETURN v_changed > 0;
END
$$;

-- Attach a project to the cell its work runs in, and optionally tighten its
-- visibility now that it has one.
--
-- The visibility argument exists for exactly one case, which 0009 wrote down at
-- the time: nged is a restricted client engagement created as confidential
-- because "the schema refuses a restricted project without one, so nged is
-- created as confidential and tightened to restricted when its cell exists".
-- Passing NULL leaves visibility alone, which is what every other project wants.
CREATE OR REPLACE FUNCTION system_attach_project_to_cell(
    p_project_slug text,
    p_cell_slug    text,
    p_visibility   text DEFAULT NULL
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_cell    uuid;
    v_changed integer;
BEGIN
    SELECT id INTO v_cell FROM execution_cells WHERE slug = p_cell_slug;
    IF v_cell IS NULL THEN
        RAISE EXCEPTION 'no execution cell named %', p_cell_slug;
    END IF;

    UPDATE projects
       SET execution_cell_id = v_cell,
           visibility        = coalesce(p_visibility, visibility),
           updated_at        = now()
     WHERE slug = p_project_slug
       AND (execution_cell_id IS DISTINCT FROM v_cell
            OR (p_visibility IS NOT NULL AND visibility IS DISTINCT FROM p_visibility));

    GET DIAGNOSTICS v_changed = ROW_COUNT;

    IF v_changed = 0 AND NOT EXISTS (SELECT 1 FROM projects WHERE slug = p_project_slug) THEN
        RAISE EXCEPTION 'no project named %', p_project_slug;
    END IF;

    RETURN v_changed > 0;
END
$$;

-- ---------------------------------------------------------------------------
-- Attribution must refuse to guess.
-- ---------------------------------------------------------------------------
--
-- 0025 and 0026 resolved a cell tag to a project with LIMIT 1. A cell can host
-- MORE than one project — docs/pilot/registry.md says portaljs-oss "may share an
-- execution cell with other non-sensitive work" — so LIMIT 1 picks one of them
-- arbitrarily and the arbitrary choice is invisible in the result.
--
-- That is the same mistake as attributing untagged spend to a default, which
-- 0025 was careful not to make: a wrong attribution is much harder to notice
-- than a missing one. So the cell resolves only when it maps to EXACTLY ONE
-- project. Where it maps to several, the row is imported with the cell recorded
-- and the project left NULL, which says "we know where this ran and not whose
-- it is" instead of naming somebody at random.
CREATE OR REPLACE FUNCTION system_record_usage(
    p_external_id text,
    p_gateway     text,
    p_provider    text,
    p_model       text,
    p_input       bigint,
    p_output      bigint,
    p_cost_cents  numeric,
    p_cached      boolean,
    p_succeeded   boolean,
    p_role        text,
    p_cell        text,
    p_rig         text,
    p_occurred_at timestamptz
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_project uuid;
    v_matches integer;
    v_new     integer;
BEGIN
    IF p_external_id IS NULL OR p_external_id = '' THEN
        RAISE EXCEPTION 'a usage record needs the gateway log id, or the import cannot be idempotent';
    END IF;

    IF p_cell IS NOT NULL AND p_cell <> '' THEN
        -- Counted first, then read. Two cheap queries rather than one clever
        -- one, because the whole point is that "more than one match" must be a
        -- visible branch and not a row that happened to come out first.
        SELECT count(*) INTO v_matches
          FROM projects p
          JOIN execution_cells c ON c.id = p.execution_cell_id
         WHERE c.slug = p_cell;

        IF v_matches = 1 THEN
            SELECT p.id INTO v_project
              FROM projects p
              JOIN execution_cells c ON c.id = p.execution_cell_id
             WHERE c.slug = p_cell;
        END IF;
    END IF;

    INSERT INTO usage_records
        (project_id, provider, model, input_tokens, output_tokens, cost_cents,
         occurred_at, gateway, external_id, cached, succeeded, role, cell, rig)
    VALUES
        (v_project, p_provider, p_model, p_input, p_output, p_cost_cents,
         p_occurred_at, p_gateway, p_external_id, p_cached, p_succeeded,
         p_role, p_cell, p_rig)
    ON CONFLICT (external_id) WHERE external_id IS NOT NULL DO NOTHING;

    GET DIAGNOSTICS v_new = ROW_COUNT;
    RETURN v_new > 0;
END
$$;

REVOKE ALL ON FUNCTION system_register_execution_node(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_register_execution_cell(text, text, text, text, integer, integer, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_attach_project_to_cell(text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_register_execution_node(text, text) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_register_execution_cell(text, text, text, text, integer, integer, integer) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_attach_project_to_cell(text, text, text) TO workgraph_app;

COMMIT;
