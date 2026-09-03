-- A bead says which project it belongs to (wg-43n).
--
-- Sixteen beads created during the 3 September demo -- the Jackson MS epic
-- wg-tzk and its fifteen children -- belonged to no project, and so did
-- sixty-four others. Not row-level security, and not the interface. The cause
-- was in system_project_bead, from 0035: attribution came from the CELL and
-- resolved only when the cell mapped to exactly one project.
--
--     cell         projects_on_cell  which
--     oss          3                 poc, portaljs-oss, roseville-poc
--
-- Creating two proof-of-concept projects on the shared oss cell switched
-- attribution off for everything in that cell, silently. "Record nothing rather
-- than guess" was the right instinct at the wrong level: the cell cannot answer
-- the question, and the bead can.
--
-- Each case below is one that was wrong, or that a later simplification would
-- get wrong.

BEGIN;

DO $$
DECLARE
  org  uuid;
  cell uuid;
  one  uuid;
  two  uuid;
  u    uuid;
  got  uuid;
  n    integer;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  -- Everything this needs is created here rather than borrowed. CI's database
  -- has no execution cells -- those arrive at deploy from the registry binary,
  -- not from a migration -- and a test that borrows staging's cells passes
  -- locally and fails in CI on a null.
  INSERT INTO execution_nodes (hostname, environment)
  VALUES ('attribution-probe.invalid', 'staging')
  ON CONFLICT (hostname) DO UPDATE SET environment = EXCLUDED.environment
  RETURNING id INTO got;

  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  VALUES (got, 'attr-shared', 'wgcell_attr_shared', 'oss')
  RETURNING id INTO cell;

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Attribution Owner', 'attr-owner@example.invalid') RETURNING id INTO u;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Attribution Backup', 'attr-backup@example.invalid') RETURNING id INTO got;

  -- TWO projects on one cell, which is what a shared sandbox is and what broke
  -- the old rule.
  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id,
                        execution_cell_id)
  VALUES (org, 'attr-one', 'Attribution One', u, got, cell) RETURNING id INTO one;
  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id,
                        execution_cell_id)
  VALUES (org, 'attr-two', 'Attribution Two', u, got, cell) RETURNING id INTO two;

  -- A project: label attributes the bead, even though the cell cannot.
  PERFORM system_project_bead('attr-shared', 'attr-1', 'Labelled', 'task', 'open',
                              ARRAY['kind:task', 'project:attr-two']);
  SELECT project_id INTO got FROM work_refs WHERE bead_id = 'attr-1';
  IF got IS DISTINCT FROM two THEN
    RAISE EXCEPTION 'a project: label did not attribute the bead (got %, wanted %)', got, two;
  END IF;

  -- Without a label, a shared cell still abstains. This is the behaviour that
  -- lost the Jackson beads, and it is CORRECT: guessing between two projects
  -- would be worse than saying nothing. The fix is that a label now exists.
  PERFORM system_project_bead('attr-shared', 'attr-2', 'Unlabelled', 'task', 'open',
                              ARRAY['kind:task']);
  SELECT project_id INTO got FROM work_refs WHERE bead_id = 'attr-2';
  IF got IS NOT NULL THEN
    RAISE EXCEPTION 'an unlabelled bead in a two-project cell was attributed to % anyway', got;
  END IF;

  -- A label naming no project is a typo, and must be loud. Recording the bead
  -- with no project would hide it in exactly the way this bead exists to fix.
  BEGIN
    PERFORM system_project_bead('attr-shared', 'attr-3', 'Typo', 'task', 'open',
                                ARRAY['project:attr-nonexistent']);
    RAISE EXCEPTION 'a label naming a project that does not exist was accepted';
  EXCEPTION WHEN others THEN
    IF position('no project has that slug' IN SQLERRM) = 0 THEN RAISE; END IF;
  END;

  -- Two project labels is a bead in two projects, which the schema cannot
  -- express. Refused rather than resolved by picking one.
  BEGIN
    PERFORM system_project_bead('attr-shared', 'attr-4', 'Both', 'task', 'open',
                                ARRAY['project:attr-one', 'project:attr-two']);
    RAISE EXCEPTION 'a bead carrying two project: labels was accepted';
  EXCEPTION WHEN others THEN
    IF position('belongs to one project' IN SQLERRM) = 0 THEN RAISE; END IF;
  END;

  -- The cell fallback still works where the cell can answer. Most cells are
  -- one-to-one with a project, and removing this would silently unattribute
  -- every bead on them.
  UPDATE projects SET execution_cell_id = NULL WHERE id = two;
  PERFORM system_project_bead('attr-shared', 'attr-5', 'Fallback', 'task', 'open', NULL);
  SELECT project_id INTO got FROM work_refs WHERE bead_id = 'attr-5';
  IF got IS DISTINCT FROM one THEN
    RAISE EXCEPTION 'the single-project cell fallback stopped working (got %, wanted %)', got, one;
  END IF;

  -- An attribution already established is not erased by a later pass that
  -- cannot attribute. Without the COALESCE in the upsert, one unlabelled
  -- projection would orphan a bead that was correctly attributed before.
  UPDATE projects SET execution_cell_id = cell WHERE id = two;
  PERFORM system_project_bead('attr-shared', 'attr-1', 'Labelled', 'task', 'closed', NULL);
  SELECT project_id INTO got FROM work_refs WHERE bead_id = 'attr-1';
  IF got IS DISTINCT FROM two THEN
    RAISE EXCEPTION 'an unlabelled pass erased an existing attribution (now %)', got;
  END IF;

  -- The unattributed count is askable, which is what makes the failure visible
  -- instead of silent. attr-2 is the one with no project.
  -- Aliased because the function's output column is also called cell, and so
  -- is the PL/pgSQL variable holding the cell id.
  SELECT u.beads INTO n FROM system_unattributed_work() u WHERE u.cell = 'attr-shared';
  IF n <> 1 THEN
    RAISE EXCEPTION 'system_unattributed_work reported % unattributed beads, wanted 1', n;
  END IF;

  RAISE NOTICE 'attribution: label wins, shared cell abstains, typo and duplicate refused, fallback intact, existing attribution preserved';
END $$;

-- ---------------------------------------------------------------------------
-- A project-scoped graph must name its project
-- ---------------------------------------------------------------------------
--
-- cell-oss was scope='project' with project_id NULL, so every bead in it was
-- unattributable however good the query. That state is now unrepresentable.
DO $$
DECLARE org uuid;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';
  BEGIN
    INSERT INTO beads_databases (organisation_id, name, scope)
    VALUES (org, 'attr-nameless-project-graph', 'project');
    RAISE EXCEPTION 'a project-scoped graph naming no project was accepted';
  EXCEPTION WHEN check_violation THEN
    NULL;
  END;
  RAISE NOTICE 'a project-scoped graph must name its project';
END $$;

ROLLBACK;
