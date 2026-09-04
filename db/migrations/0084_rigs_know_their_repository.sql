-- A rig knows which repository it holds, so a dispatch can be routed or refused
-- (wg-ugb).
--
-- Three PortalJS beads were dispatched on 4 September and ran against
-- datopian/workgraph-agent-sandbox, because dispatch has no repository routing:
--
--   POST /v1/work/{bead}/dispatch defaults the cell to "oss" and passes rig
--   through unset, and cmd/dispatcher then falls back to its own default rig,
--   "sandbox". The MCP tool sends an empty body, so EVERY dispatch from a
--   connector landed there.
--
-- The agent did the right thing and said so: "found no PortalJS source
-- anywhere... The repo itself is labeled 'Disposable target for Workgraph agent
-- runs.'" Cost of learning that: 48 model calls and about 78 cents.
--
-- project_repositories already knows portaljs holds datopian/portaljs and
-- datopian/cloud.portaljs.com. What was missing is the other half of the join:
-- nothing recorded which repository a RIG works on. Rigs were strings on
-- work_queue and usage_records and nothing else.
--
-- So the node reports it. The dispatcher can read its own rig's git remote --
-- it is sitting in the working tree -- and that is the only place the truth
-- lives, which is why it is reported upward rather than configured twice.

BEGIN;

CREATE TABLE IF NOT EXISTS execution_rigs (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    execution_cell_id uuid NOT NULL REFERENCES execution_cells(id) ON DELETE CASCADE,
    -- The rig's directory name under the cell's town, which is what work_queue
    -- and usage_records already record as `rig`.
    rig               text NOT NULL,
    -- The repository its working tree is a checkout of. Nullable: a rig may
    -- legitimately hold none -- the witness and the mayor are rigs with no
    -- repository -- and a NULL here says "reports no repository" rather than
    -- "not yet asked", which last_seen_at answers.
    provider          text CHECK (provider IS NULL OR provider IN ('github')),
    owner             text,
    name              text,
    -- When the node last said so. A rig that stops reporting is a rig that may
    -- have been removed, and routing to it would be routing into the past.
    last_seen_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (execution_cell_id, rig),
    -- Either a whole repository or none of one. A row with an owner and no name
    -- cannot be joined to project_repositories and would silently match
    -- nothing.
    CONSTRAINT rig_repository_is_whole
        CHECK ((provider IS NULL AND owner IS NULL AND name IS NULL)
            OR (provider IS NOT NULL AND owner IS NOT NULL AND name IS NOT NULL))
);

COMMENT ON TABLE execution_rigs IS
    'Which repository each rig''s working tree holds, reported by the node. '
    'The join that lets a dispatch be routed to a rig that can do the work, '
    'or refused (wg-ugb).';

ALTER TABLE execution_rigs ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_rigs FORCE ROW LEVEL SECURITY;

-- Readable by any authenticated caller, like execution_cells: a rig name and a
-- public repository name are not secrets, and the dispatch path needs to read
-- them to explain a refusal. Written only through the function below.
CREATE POLICY execution_rigs_read ON execution_rigs FOR SELECT
    USING (current_app_user() IS NOT NULL);

GRANT SELECT ON execution_rigs TO workgraph_app;

-- ---------------------------------------------------------------------------
-- The node reports what it has
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION system_register_rig(
    p_cell text, p_rig text,
    p_provider text DEFAULT NULL, p_owner text DEFAULT NULL, p_name text DEFAULT NULL
) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
DECLARE v_cell uuid;
BEGIN
    SELECT id INTO v_cell FROM execution_cells WHERE slug = p_cell;
    IF v_cell IS NULL THEN
        RAISE EXCEPTION 'no execution cell named %; register it first', p_cell;
    END IF;
    IF coalesce(trim(p_rig), '') = '' THEN
        RAISE EXCEPTION 'a rig needs a name';
    END IF;

    INSERT INTO execution_rigs (execution_cell_id, rig, provider, owner, name, last_seen_at)
    VALUES (v_cell, trim(p_rig), nullif(trim(coalesce(p_provider,'')), ''),
            nullif(trim(coalesce(p_owner,'')), ''), nullif(trim(coalesce(p_name,'')), ''),
            now())
    ON CONFLICT (execution_cell_id, rig)
    DO UPDATE SET provider = EXCLUDED.provider,
                  owner = EXCLUDED.owner,
                  name = EXCLUDED.name,
                  -- Replaced rather than COALESCEd, unlike a bead's comment: a
                  -- rig that has been re-pointed at a different repository must
                  -- stop claiming the old one, and "reports none now" is a fact
                  -- worth recording rather than an absence to paper over.
                  last_seen_at = now();
    RETURN true;
END
$$;

REVOKE ALL ON FUNCTION system_register_rig(text, text, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_register_rig(text, text, text, text, text) TO workgraph_app;

-- ---------------------------------------------------------------------------
-- Where a bead should run
-- ---------------------------------------------------------------------------
--
-- Returns the rigs in a cell that hold a repository belonging to the bead's
-- project, newest report first. Empty means "nowhere in this cell can do this
-- work", which is what dispatch refuses on.
--
-- A bead with NO project is not routed and not refused: company-wide work has
-- no repository to match and the caller's chosen rig stands. That is the
-- pre-existing behaviour and this change deliberately does not alter it.
CREATE OR REPLACE FUNCTION system_rigs_for_bead(p_bead text, p_cell text)
RETURNS TABLE (rig text, repository text, last_seen_at timestamptz)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT r.rig,
           r.owner || '/' || r.name,
           r.last_seen_at
      FROM execution_rigs r
      JOIN execution_cells c ON c.id = r.execution_cell_id
      JOIN project_repositories pr
        ON pr.provider = r.provider AND pr.owner = r.owner AND pr.name = r.name
      JOIN work_refs w
        ON w.project_id = pr.project_id
     WHERE c.slug = p_cell
       AND w.bead_id = p_bead
       AND r.provider IS NOT NULL
     ORDER BY r.last_seen_at DESC;
$$;

REVOKE ALL ON FUNCTION system_rigs_for_bead(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_rigs_for_bead(text, text) TO workgraph_app;

COMMIT;
