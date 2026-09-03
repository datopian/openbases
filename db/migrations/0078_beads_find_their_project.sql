-- A bead can say which project it belongs to (wg-43n).
--
-- wg:backfill — one UPDATE, moving existing cell graphs to scope 'cell' before
-- the constraint that would refuse them. It touches no user data: it corrects
-- how a graph created by system_project_bead describes itself.
--
-- Sixteen beads created during the demo -- the Jackson MS epic wg-tzk and its
-- fifteen children -- are attributed to no project. So are sixty-four others.
-- The cause is not row-level security and not the interface: it is this rule in
-- system_project_bead, added in 0035.
--
--     SELECT count(*) INTO v_matches FROM projects p
--      WHERE p.execution_cell_id = v_cell;
--     IF v_matches = 1 THEN ... END IF;
--
-- Attribution came from the CELL, and resolved only when the cell mapped to
-- exactly one project. That held while cells were one-to-one with projects. It
-- stopped holding the moment a cell was shared:
--
--     cell         projects_on_cell  which
--     oss          3                 poc, portaljs-oss, roseville-poc
--     client-cdt   1                 cdt
--     client-nged  1                 nged
--
-- Creating the two proof-of-concept projects on the oss cell silently switched
-- attribution off for every bead in that cell. 130 beads projected before that
-- carry a project; 80 projected after carry none. Nothing reported it, because
-- "record nothing rather than guess" is the correct instinct applied at the
-- wrong level: the cell cannot answer the question, but the bead can.
--
-- A shared cell is not a mistake to be designed out. A standing sandbox for
-- proof-of-concept work is exactly one cell serving many projects, and that is
-- the arrangement wg-rbg deliberately set up.
--
-- So the bead answers instead, through the label convention the graph already
-- uses -- scope:company, portfolio:internal, visibility:internal, kind:task --
-- extended with project:<slug>. The cell rule stays as a fallback for the
-- one-to-one case, which is most cells.

BEGIN;

-- ---------------------------------------------------------------------------
-- A graph that serves a whole cell is not project-scoped
-- ---------------------------------------------------------------------------
--
-- system_project_bead creates the cell's graph on first sight with
-- scope = 'project' and no project_id. For a shared cell that is not merely
-- unset, it is unrepresentable: one graph serves three projects, so there is no
-- project it could name.
--
-- 'cell' says what it actually is. With it, the constraint below can require a
-- project-scoped graph to name its project, which mirrors
-- personal_scope_has_owner and closes the same kind of hole.

ALTER TABLE beads_databases DROP CONSTRAINT IF EXISTS beads_databases_scope_check;
ALTER TABLE beads_databases ADD CONSTRAINT beads_databases_scope_check
    CHECK (scope IN ('company', 'project', 'function', 'personal', 'cell'));

-- Existing cell graphs, before the constraint that would refuse them.
UPDATE beads_databases
   SET scope = 'cell'
 WHERE scope = 'project'
   AND project_id IS NULL
   AND execution_cell_id IS NOT NULL;

-- Now it can be required. A project-scoped graph that names no project is a
-- graph whose every bead is unattributable, which is the state the Jackson
-- beads were in.
ALTER TABLE beads_databases DROP CONSTRAINT IF EXISTS project_scope_names_project;
ALTER TABLE beads_databases ADD CONSTRAINT project_scope_names_project
    CHECK (scope <> 'project' OR project_id IS NOT NULL);

-- ---------------------------------------------------------------------------
-- Attribution, from the bead
-- ---------------------------------------------------------------------------
--
-- DROP before CREATE, not CREATE OR REPLACE with a defaulted parameter. Adding
-- a parameter to an existing function creates a SECOND overload, and every
-- prior-arity call then fails with "is not unique" -- which is how
-- system_enqueue_work broke on a deploy once already. The old five-argument
-- form must go.
-- BOTH signatures, not only the old one.
--
-- Dropping the five-argument form and then CREATE-ing the six-argument form is
-- not re-runnable: the second attempt fails with "function
-- system_project_bead already exists with same argument types" (42723). That
-- is exactly what happened -- this migration's DDL reached staging through an
-- out-of-band probe without a tracking row, and the deploy that should have
-- recorded it failed on its own work instead.
--
-- A migration normally runs once, so this is not a rule the tool enforces. It
-- costs one line, and the line is the difference between a deploy that
-- converges and a deploy that has to be unpicked by hand.
DROP FUNCTION IF EXISTS system_project_bead(text, text, text, text, text);
DROP FUNCTION IF EXISTS system_project_bead(text, text, text, text, text, text[]);

CREATE FUNCTION system_project_bead(
    p_cell text, p_bead text, p_title text, p_kind text, p_status text,
    p_labels text[] DEFAULT NULL
) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
DECLARE
    v_cell uuid; v_org uuid; v_db uuid; v_project uuid; v_matches integer;
    v_slug text;
BEGIN
    SELECT id INTO v_cell FROM execution_cells WHERE slug = p_cell;
    IF v_cell IS NULL THEN
        RAISE EXCEPTION 'no execution cell named %; register it first', p_cell;
    END IF;

    SELECT id INTO v_org FROM organisations ORDER BY created_at LIMIT 1;

    -- The cell's graph, created on first sight rather than requiring a separate
    -- registration step for something the projection can infer. Now 'cell'
    -- rather than 'project': it serves the cell, and may serve several
    -- projects through it.
    -- Ordered scope='project' FIRST, which looks backwards and is not.
    --
    -- work_refs' natural key includes beads_database_id, so changing which
    -- graph a cell's beads belong to does not move them: it inserts a second
    -- copy under the new graph and leaves the first behind. A probe against
    -- staging created cell-client-cdt beside the existing project-cdt and this
    -- lookup, ordered 'cell' first, would have duplicated every cdt bead on
    -- the next projection pass.
    --
    -- So an existing project-scoped graph keeps the beads it already holds,
    -- and 'cell' is only what a NEW graph is created as.
    SELECT id INTO v_db FROM beads_databases
     WHERE execution_cell_id = v_cell AND scope IN ('cell', 'project')
     ORDER BY (scope = 'project') DESC LIMIT 1;
    IF v_db IS NULL THEN
        INSERT INTO beads_databases (organisation_id, execution_cell_id, name, scope)
        VALUES (v_org, v_cell, 'cell-' || p_cell, 'cell')
        RETURNING id INTO v_db;
    END IF;

    -- What the bead says about itself, first.
    --
    -- One project: label per bead. Several would be a bead belonging to two
    -- projects, which the schema cannot express and which would silently pick
    -- one -- so it is refused loudly instead. A label naming a project that
    -- does not exist is a typo and gets the same treatment: recording the bead
    -- with no project would hide it exactly as before.
    IF p_labels IS NOT NULL THEN
        SELECT count(*), min(substring(l from 9))
          INTO v_matches, v_slug
          FROM unnest(p_labels) l
         WHERE l LIKE 'project:%';

        IF v_matches > 1 THEN
            RAISE EXCEPTION 'bead % carries % project: labels; a bead belongs to one project',
                p_bead, v_matches;
        END IF;

        IF v_matches = 1 THEN
            SELECT id INTO v_project FROM projects WHERE slug = v_slug;
            IF v_project IS NULL THEN
                RAISE EXCEPTION 'bead % is labelled project:%, and no project has that slug',
                    p_bead, v_slug;
            END IF;
        END IF;
    END IF;

    -- Failing that, the cell -- but only where the cell can actually answer.
    -- Unchanged in substance from 0035, and now a fallback rather than the only
    -- mechanism, so a shared cell no longer means no attribution at all.
    IF v_project IS NULL THEN
        SELECT count(*) INTO v_matches FROM projects p WHERE p.execution_cell_id = v_cell;
        IF v_matches = 1 THEN
            SELECT p.id INTO v_project FROM projects p WHERE p.execution_cell_id = v_cell;
        END IF;
    END IF;

    INSERT INTO work_refs (organisation_id, execution_cell_id, beads_database_id,
                           bead_id, title, kind, status, project_id, last_seen_at)
    VALUES (v_org, v_cell, v_db, p_bead, p_title, p_kind, p_status, v_project, now())
    ON CONFLICT (organisation_id, execution_cell_id, beads_database_id, bead_id)
    DO UPDATE SET title = EXCLUDED.title,
                  kind = EXCLUDED.kind,
                  status = EXCLUDED.status,
                  -- COALESCE so a pass that cannot attribute does not erase an
                  -- attribution an earlier pass established. It also means a
                  -- label added later sticks, while removing one does not
                  -- silently orphan a bead.
                  project_id = COALESCE(EXCLUDED.project_id, work_refs.project_id),
                  last_seen_at = now();
    RETURN true;
END
$$;

REVOKE ALL ON FUNCTION system_project_bead(text, text, text, text, text, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_project_bead(text, text, text, text, text, text[]) TO workgraph_app;

-- ---------------------------------------------------------------------------
-- What is unattributed, so it stops being silent
-- ---------------------------------------------------------------------------
--
-- The whole failure was invisibility: eighty beads lost their project and the
-- only symptom was an empty column on a page nobody reads column-by-column.
-- This makes the count askable, which is what the platform view (wg-7bh) and
-- the monitor need in order to say so out loud.
CREATE OR REPLACE FUNCTION system_unattributed_work()
RETURNS TABLE (cell text, graph text, projects_on_cell bigint, beads bigint)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp
AS $$
    SELECT c.slug,
           b.name,
           (SELECT count(*) FROM projects p WHERE p.execution_cell_id = c.id),
           count(*)
      FROM work_refs w
      JOIN execution_cells c ON c.id = w.execution_cell_id
      LEFT JOIN beads_databases b ON b.id = w.beads_database_id
     WHERE w.project_id IS NULL
     GROUP BY c.slug, b.name, c.id
     ORDER BY count(*) DESC;
$$;

REVOKE ALL ON FUNCTION system_unattributed_work() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_unattributed_work() TO workgraph_app;

COMMIT;
