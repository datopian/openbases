-- A bead says what blocks it, and the direction is not inverted.
--
-- work_links has existed since 0001 with a 'blocks' relation and never held a
-- row in production: nothing projected dependencies, so the only writer was the
-- test suite. `bd list --json` has been returning them all along.
--
-- The direction is what this test is really for. Beads records
-- {issue_id: X, depends_on_id: Y} -- X depends on Y -- and work_links records
-- from BLOCKS to, so Y is `from` and X is `to`. Inverting that would show every
-- reader the exact opposite of the truth: work that is ready reported as
-- blocked, and work that cannot start reported as available. Asserted with
-- named beads rather than trusted to a comment.
--
-- Owner-only by design, recorded in scripts/check_rls_tests.py: every write is
-- through a SECURITY DEFINER function that the node calls with no app user, and
-- the assertions are about which edges it recorded.
\set ON_ERROR_STOP on

BEGIN;

DO $$
DECLARE
  org uuid; node uuid; cell uuid; db uuid; u uuid; backup uuid; proj uuid;
  n integer; unresolved integer; ids text[];
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  INSERT INTO execution_nodes (hostname, environment)
  VALUES ('blockers-probe.invalid', 'staging')
  ON CONFLICT (hostname) DO UPDATE SET environment = EXCLUDED.environment
  RETURNING id INTO node;
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  VALUES (node, 'blk-cell', 'wgcell_blk', 'oss') RETURNING id INTO cell;

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Blk Owner', 'blk-owner@example.invalid') RETURNING id INTO u;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Blk Backup', 'blk-backup@example.invalid') RETURNING id INTO backup;
  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id,
                        execution_cell_id)
  VALUES (org, 'blk-proj', 'Blocked Project', u, backup, cell) RETURNING id INTO proj;

  INSERT INTO beads_databases (organisation_id, execution_cell_id, name, scope, project_id)
  VALUES (org, cell, 'blk-graph', 'project', proj) RETURNING id INTO db;

  -- Three beads: blk-2 depends on blk-1, so blk-1 blocks blk-2. blk-3 is free.
  INSERT INTO work_refs (organisation_id, beads_database_id, execution_cell_id, bead_id,
                         title, kind, status, project_id)
  VALUES (org, db, cell, 'blk-1', 'the blocker', 'task', 'open', proj),
         (org, db, cell, 'blk-2', 'the blocked', 'task', 'open', proj),
         (org, db, cell, 'blk-3', 'free work',   'task', 'open', proj);

  -- blk-2 depends on blk-1.
  SELECT system_project_bead_blockers('blk-cell', 'blk-2', ARRAY['blk-1']) INTO unresolved;
  IF unresolved <> 0 THEN
      RAISE EXCEPTION 'a blocker in the same graph was reported unresolved';
  END IF;

  -- THE DIRECTION. from = the blocker, to = the blocked.
  SELECT count(*) INTO n
    FROM work_links l
    JOIN work_refs f ON f.id = l.from_work_ref
    JOIN work_refs t ON t.id = l.to_work_ref
   WHERE l.relation = 'blocks' AND f.bead_id = 'blk-1' AND t.bead_id = 'blk-2';
  IF n <> 1 THEN
      RAISE EXCEPTION 'the edge is not recorded as blk-1 blocks blk-2; a reader would '
          'be shown the opposite of the truth';
  END IF;

  -- And not the other way round, which is the failure that looks identical
  -- until somebody reads it.
  SELECT count(*) INTO n
    FROM work_links l
    JOIN work_refs f ON f.id = l.from_work_ref
    JOIN work_refs t ON t.id = l.to_work_ref
   WHERE l.relation = 'blocks' AND f.bead_id = 'blk-2' AND t.bead_id = 'blk-1';
  IF n <> 0 THEN
      RAISE EXCEPTION 'the edge was recorded inverted';
  END IF;

  -- A dependency removed in Beads disappears here. The projection sends the
  -- current list every pass, so an edge that is no longer sent must go.
  SELECT system_project_bead_blockers('blk-cell', 'blk-2', ARRAY[]::text[]) INTO unresolved;
  SELECT count(*) INTO n FROM work_links WHERE relation = 'blocks';
  IF n <> 0 THEN
      RAISE EXCEPTION 'removing a dependency left % edge(s) behind', n;
  END IF;

  -- An unresolvable blocker is counted, not fatal. Beads writes a cross-graph
  -- reference as external:<prefix>:<id>, and that bead is not projected here.
  SELECT system_project_bead_blockers('blk-cell', 'blk-2',
      ARRAY['external:sa:sa-cx9', 'blk-1']) INTO unresolved;
  IF unresolved <> 1 THEN
      RAISE EXCEPTION 'expected one unresolved blocker, got %', unresolved;
  END IF;
  -- And the one that DID resolve was still recorded, rather than the whole
  -- call being abandoned.
  SELECT count(*) INTO n FROM work_links WHERE relation = 'blocks';
  IF n <> 1 THEN
      RAISE EXCEPTION 'a resolvable blocker was dropped alongside an unresolvable one';
  END IF;

  -- A self-dependency is skipped rather than raising: it is a mistake in the
  -- graph, and the table's own constraint would refuse it anyway.
  SELECT system_project_bead_blockers('blk-cell', 'blk-3', ARRAY['blk-3']) INTO unresolved;

  -- A bead that is not projected has no links to record, and that is not an
  -- error: the caller projects beads, then links, and may have failed on one.
  SELECT system_project_bead_blockers('blk-cell', 'blk-nope', ARRAY['blk-1']) INTO unresolved;

  -- And the read path the interface uses: the arrays come back in both
  -- directions.
  SELECT COALESCE(ARRAY(SELECT f.bead_id FROM work_links l
                          JOIN work_refs f ON f.id = l.from_work_ref
                         WHERE l.to_work_ref = (SELECT id FROM work_refs WHERE bead_id='blk-2')
                           AND l.relation='blocks'), '{}') INTO ids;
  IF ids <> ARRAY['blk-1'] THEN
      RAISE EXCEPTION 'blocked_by for blk-2 reads as %, not {blk-1}', ids::text;
  END IF;
END
$$;

ROLLBACK;

SELECT 'a bead says what blocks it, in the right direction' AS result;
