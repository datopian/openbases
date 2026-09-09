-- Dependencies between beads reach the interface.
--
-- work_links has existed since 0001 with a 'blocks' relation, is covered by two
-- integration tests, and has never held a row in production: nothing projected
-- dependencies, so the only writer was the test suite. Meanwhile `bd list
-- --json` has been returning them all along -- `dependencies`, with a
-- `depends_on_id` and a `type` -- and the projection read past them.
--
-- The effect on a reader: the project page lists beads in a flat table, so work
-- that cannot start looks exactly like work nobody has picked up. Twelve of the
-- fifty beads in the oss cell have a dependency.
--
-- DIRECTION, stated once here because inverting it inverts every blocker shown
-- to a person:
--
--     from_work_ref BLOCKS to_work_ref
--
-- Beads records the opposite way round -- {issue_id: X, depends_on_id: Y} means
-- X depends on Y -- so Y is `from` and X is `to`. The test asserts this with
-- named beads rather than trusting the comment.
BEGIN;

DROP FUNCTION IF EXISTS system_project_bead_blockers(text, text, text[]);

-- Records which beads block one bead, replacing whatever was recorded before.
--
-- Called after every bead in a projection batch, never during: a link needs
-- both work_refs rows, and a blocker may be projected later in the same batch.
-- Called mid-batch it would silently drop every forward reference.
--
-- Returns the number of blockers it could not resolve, which is not an error.
-- Beads expresses cross-graph references as `external:<prefix>:<id>` and a
-- blocker may live in a graph this cell does not project; the edge is simply
-- not recordable here, and reporting the count is how that stays visible
-- instead of looking like an empty dependency list.
CREATE OR REPLACE FUNCTION system_project_bead_blockers(
    p_cell     text,
    p_bead     text,
    p_blockers text[]
) RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_cell      uuid;
    v_to        uuid;
    v_from      uuid;
    v_blocker   text;
    v_unresolved integer := 0;
BEGIN
    SELECT id INTO v_cell FROM execution_cells WHERE slug = p_cell;
    IF v_cell IS NULL THEN
        RAISE EXCEPTION 'no execution cell named %', p_cell;
    END IF;

    SELECT w.id INTO v_to
      FROM work_refs w
      JOIN beads_databases b ON b.id = w.beads_database_id
     WHERE w.bead_id = p_bead AND b.execution_cell_id = v_cell
     LIMIT 1;
    IF v_to IS NULL THEN
        -- The bead itself is not projected. Not an error: the caller projects
        -- beads and then their links, and a bead it failed to project has no
        -- links to record.
        RETURN 0;
    END IF;

    -- Removed first, so a dependency deleted in Beads disappears here. Scoped
    -- to 'blocks' edges pointing AT this bead, which are the only ones this
    -- function owns -- an 'implements' link recorded by a person must survive.
    DELETE FROM work_links l
     WHERE l.to_work_ref = v_to
       AND l.relation = 'blocks'
       AND (p_blockers IS NULL OR NOT EXISTS (
             SELECT 1 FROM unnest(p_blockers) AS b(id)
              JOIN work_refs w ON w.bead_id = b.id
             WHERE w.id = l.from_work_ref));

    IF p_blockers IS NULL THEN
        RETURN 0;
    END IF;

    FOREACH v_blocker IN ARRAY p_blockers
    LOOP
        -- Not scoped to this cell: a blocker may legitimately live in another
        -- graph, and recording that edge is the whole reason work_links exists
        -- rather than relying on Beads, which cannot reference across
        -- databases (plan section 2.3).
        SELECT id INTO v_from FROM work_refs WHERE bead_id = v_blocker LIMIT 1;
        IF v_from IS NULL THEN
            v_unresolved := v_unresolved + 1;
            CONTINUE;
        END IF;
        IF v_from = v_to THEN
            -- A self-dependency would make a readiness walk loop, and the
            -- table's own constraint refuses it. Skipped quietly rather than
            -- raising: it is a mistake in the graph, not in this call.
            CONTINUE;
        END IF;
        INSERT INTO work_links (from_work_ref, to_work_ref, relation)
        VALUES (v_from, v_to, 'blocks')
        ON CONFLICT (from_work_ref, to_work_ref, relation) DO NOTHING;
    END LOOP;

    RETURN v_unresolved;
END
$$;

REVOKE ALL ON FUNCTION system_project_bead_blockers(text, text, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_project_bead_blockers(text, text, text[]) TO workgraph_app;

COMMIT;
