-- Cross-graph link layer and work-reference identity (WP-D2).
--
-- The property under test is the one that fails silently: work_refs identity
-- must hold when execution_cell_id is NULL, which is the normal case for a
-- company or personal graph belonging to no cell. Postgres treats NULLs as
-- distinct, so the original UNIQUE constraint never matched and the same bead
-- was inserted repeatedly — each new row orphaning the links that pointed at
-- the previous one.

BEGIN;

DO $$
DECLARE org uuid; hq uuid; proj uuid; pid uuid; a uuid; b uuid; n integer;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';
  -- datahub, because 0080 deleted datopian-products when it dissolved the
  -- products project.
  --
  -- Asserted rather than assumed. When the project vanished this silently became
  -- NULL, and the project-scoped graph below was then inserted naming no
  -- project -- which is exactly the state wg-43n was about, where every bead in
  -- a graph is unattributable. The project_scope_names_project constraint added
  -- in 0078 is what caught it, and this check is so the next failure says
  -- "the project is missing" instead of "a constraint was violated".
  SELECT id INTO pid FROM projects WHERE slug = 'datahub';
  IF pid IS NULL THEN
    RAISE EXCEPTION 'the datahub project is missing, so this test has no project to attach a graph to';
  END IF;

  -- A company graph, deliberately with NO execution cell.
  INSERT INTO beads_databases (organisation_id, name, scope, path)
  VALUES (org, 'company-hq-test', 'company', '/srv/graphs/hq')
  RETURNING id INTO hq;

  INSERT INTO beads_databases (organisation_id, project_id, name, scope, path)
  VALUES (org, pid, 'products-test', 'project', '/srv/cells/oss/products')
  RETURNING id INTO proj;

  -- Same bead recorded twice. With the original constraint this inserted two
  -- rows, because execution_cell_id is NULL on both.
  INSERT INTO work_refs (organisation_id, beads_database_id, bead_id, title)
  VALUES (org, hq, 'hq-1', 'A company commitment')
  ON CONFLICT (organisation_id,
               COALESCE(execution_cell_id, '00000000-0000-0000-0000-000000000000'::uuid),
               beads_database_id, bead_id)
  DO UPDATE SET last_seen_at = now()
  RETURNING id INTO a;

  INSERT INTO work_refs (organisation_id, beads_database_id, bead_id, title)
  VALUES (org, hq, 'hq-1', 'A company commitment')
  ON CONFLICT (organisation_id,
               COALESCE(execution_cell_id, '00000000-0000-0000-0000-000000000000'::uuid),
               beads_database_id, bead_id)
  DO UPDATE SET last_seen_at = now()
  RETURNING id INTO b;

  IF a <> b THEN
    RAISE EXCEPTION 'a cell-less work reference was inserted twice; links would be orphaned';
  END IF;

  SELECT count(*) INTO n FROM work_refs WHERE beads_database_id = hq AND bead_id = 'hq-1';
  IF n <> 1 THEN RAISE EXCEPTION 'expected 1 work_ref, found %', n; END IF;

  -- The cross-graph edge Beads cannot express: a company commitment
  -- implemented by project work in a different database.
  INSERT INTO work_refs (organisation_id, beads_database_id, bead_id, title, project_id)
  VALUES (org, proj, 'px-9', 'Implement the thing', pid);

  INSERT INTO work_links (from_work_ref, to_work_ref, relation)
  SELECT a, w.id, 'implements' FROM work_refs w WHERE w.bead_id = 'px-9';

  SELECT count(*) INTO n
  FROM work_links l JOIN work_refs f ON f.id = l.from_work_ref
                    JOIN work_refs t ON t.id = l.to_work_ref
  WHERE f.beads_database_id <> t.beads_database_id;
  IF n <> 1 THEN RAISE EXCEPTION 'the cross-database link was not recorded'; END IF;

  -- Self-links are refused: a dependency on itself is never meaningful and
  -- would make a readiness walk loop.
  BEGIN
    INSERT INTO work_links (from_work_ref, to_work_ref, relation) VALUES (a, a, 'relates');
    RAISE EXCEPTION 'a self-link was accepted';
  EXCEPTION WHEN check_violation THEN NULL;
  END;

  -- The same edge twice must not duplicate.
  BEGIN
    INSERT INTO work_links (from_work_ref, to_work_ref, relation)
    SELECT a, w.id, 'implements' FROM work_refs w WHERE w.bead_id = 'px-9';
    RAISE EXCEPTION 'a duplicate link was accepted';
  EXCEPTION WHEN unique_violation THEN NULL;
  END;

  -- A path must be recorded, or the adapter cannot know where to run bd.
  SELECT count(*) INTO n FROM beads_databases WHERE id IN (hq, proj) AND path IS NULL;
  IF n <> 0 THEN RAISE EXCEPTION '% registered database(s) have no path', n; END IF;

  RAISE NOTICE 'hq graph assertions passed';
END
$$;

ROLLBACK;
