-- Attribution reads the project label this codebase already uses (wg-43n).
--
-- 0078 taught system_project_bead to attribute a bead from a `project:<slug>`
-- label. That spelling was invented there, and it should not have been: a
-- project label already existed, with three producers and tests behind it.
--
--   internal/work/work.go:97       the planning prompt tells the agent
--                                  "Label every bead you create with
--                                   `wg-project-<slug>`"
--   internal/publish/publish.go:221    labels = append(labels, "wg-project-"+c.ProjectSlug)
--   internal/publish/records.go:414    issue.Labels = append(issue.Labels, "wg-project-"+...)
--
-- So the producer and the consumer disagreed, silently, and a plan job filed
-- from a connector on 4 September produced sixteen beads that no project page
-- could show. The two halves that had to agree were spelled differently, which
-- is the failure this repository writes tests against everywhere else.
--
-- wg-project- wins because it was here first and has more producers. Both are
-- accepted for one migration's worth of overlap, because sixteen Jackson beads
-- were hand-labelled `project:bizdev` while the wrong spelling was believed to
-- be the convention; they are relabelled below, and the `project:` branch then
-- has no producer left. It is kept rather than dropped so that a bead labelled
-- during that window is not silently orphaned by this migration -- the exact
-- thing being fixed.
--
-- wg:backfill -- relabels nothing in SQL (labels live in Beads, not here) but
-- does re-attribute the work_refs rows whose beads carry the older spelling.

BEGIN;

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
    v_slug text; v_slugs text[];
BEGIN
    SELECT id INTO v_cell FROM execution_cells WHERE slug = p_cell;
    IF v_cell IS NULL THEN
        RAISE EXCEPTION 'no execution cell named %; register it first', p_cell;
    END IF;

    SELECT id INTO v_org FROM organisations ORDER BY created_at LIMIT 1;

    SELECT id INTO v_db FROM beads_databases
     WHERE execution_cell_id = v_cell AND scope IN ('cell', 'project')
     ORDER BY (scope = 'project') DESC LIMIT 1;
    IF v_db IS NULL THEN
        INSERT INTO beads_databases (organisation_id, execution_cell_id, name, scope)
        VALUES (v_org, v_cell, 'cell-' || p_cell, 'cell')
        RETURNING id INTO v_db;
    END IF;

    -- What the bead says about itself.
    --
    -- Both spellings, with wg-project- the canonical one. Collected into an
    -- array first so that "two project labels" is detected across BOTH forms:
    -- a bead carrying wg-project-a and project:b belongs to two projects just
    -- as surely as one carrying two of either, and picking one silently is the
    -- outcome worth refusing.
    IF p_labels IS NOT NULL THEN
        SELECT array_agg(DISTINCT s) INTO v_slugs FROM (
            SELECT substring(l from 12) AS s FROM unnest(p_labels) l
             WHERE l LIKE 'wg-project-%'
            UNION ALL
            SELECT substring(l from 9) AS s FROM unnest(p_labels) l
             WHERE l LIKE 'project:%'
        ) found WHERE s IS NOT NULL AND s <> '';

        v_matches := coalesce(array_length(v_slugs, 1), 0);

        IF v_matches > 1 THEN
            RAISE EXCEPTION 'bead % names % projects (%); a bead belongs to one project',
                p_bead, v_matches, array_to_string(v_slugs, ', ');
        END IF;

        IF v_matches = 1 THEN
            v_slug := v_slugs[1];
            SELECT id INTO v_project FROM projects WHERE slug = v_slug;
            IF v_project IS NULL THEN
                RAISE EXCEPTION 'bead % is labelled for project %, and no project has that slug',
                    p_bead, v_slug;
            END IF;
        END IF;
    END IF;

    -- Failing that, the cell -- but only where the cell can answer.
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
                  project_id = COALESCE(EXCLUDED.project_id, work_refs.project_id),
                  last_seen_at = now();
    RETURN true;
END
$$;

REVOKE ALL ON FUNCTION system_project_bead(text, text, text, text, text, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_project_bead(text, text, text, text, text, text[]) TO workgraph_app;

COMMIT;
