-- Per-bead budgets: resolution, summation, and refusing to guess (wg-qw1).
--
-- The pairing under test is that "which budget applies" and "what counts
-- against it" agree. A bead budget must be compared against that bead's spend
-- and a project budget against the project's whole spend — if the resolution
-- and the summation ever disagree, ten beads can each spend the project's
-- entire budget and nothing trips.

BEGIN;

-- Fixtures: a cell, a project on it, and two beads in that project.
DO $$
DECLARE cell uuid; proj uuid; org uuid; db_id uuid;
BEGIN
  PERFORM system_register_execution_node('wg-test-budget-node', 'staging');
  PERFORM system_register_execution_cell(
    'wg-test-budget-node', 'wg-test-budget-cell', 'wgcell_budget', 'oss', 2, 100, 2048);
  SELECT id INTO cell FROM execution_cells WHERE slug = 'wg-test-budget-cell';

  SELECT id INTO org FROM organisations WHERE slug = 'datopian';
  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id, execution_cell_id)
  SELECT org, 'wg-test-budget-project', 'Budget Test',
         (SELECT id FROM users WHERE primary_email = 'anuar.ustayev@datopian.com'),
         (SELECT id FROM users WHERE primary_email = 'rufus.pollock@datopian.com'),
         cell;
  SELECT id INTO proj FROM projects WHERE slug = 'wg-test-budget-project';

  INSERT INTO beads_databases (organisation_id, execution_cell_id, project_id, name, scope)
  VALUES (org, cell, proj, 'wg-test-budget-graph', 'project')
  RETURNING id INTO db_id;

  INSERT INTO work_refs (organisation_id, execution_cell_id, beads_database_id, bead_id, title, project_id)
  VALUES (org, cell, db_id, 'wg-test-a', 'Budget test A', proj),
         (org, cell, db_id, 'wg-test-b', 'Budget test B', proj);
END
$$;

-- ---------------------------------------------------------------------------
-- No budget anywhere is its own answer, not an unlimited one
-- ---------------------------------------------------------------------------
DO $$
DECLARE kind text; lim numeric;
BEGIN
  SELECT subject_kind, daily_cents INTO kind, lim
    FROM system_budget_status('wg-test-a', 'wg-test-budget-cell');
  IF kind <> 'none' OR lim IS NOT NULL THEN
    RAISE EXCEPTION 'an unbudgeted bead reported kind=% limit=%, expected none/NULL', kind, lim;
  END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- Setting, and re-setting
-- ---------------------------------------------------------------------------
DO $$
DECLARE n integer; lim numeric;
BEGIN
  PERFORM system_set_budget('cell', 'wg-test-budget-cell', 10000);
  PERFORM system_set_budget('project', 'wg-test-budget-project', 5000);
  PERFORM system_set_budget('bead', 'wg-test-a', 200);

  -- Re-setting must update the one row rather than adding a second. Two rows
  -- for one subject both apply, and which wins is whichever the planner
  -- returns first.
  PERFORM system_set_budget('bead', 'wg-test-a', 250.5);
  SELECT count(*) INTO n FROM budget_limits b
    JOIN work_refs w ON w.id = b.work_ref_id WHERE w.bead_id = 'wg-test-a';
  IF n <> 1 THEN RAISE EXCEPTION 'setting a bead budget twice produced % rows', n; END IF;

  SELECT daily_cents INTO lim FROM system_budget_status('wg-test-a', 'wg-test-budget-cell');
  IF lim <> 250.5 THEN RAISE EXCEPTION 'the budget did not update: %', lim; END IF;

  -- A budget that cannot express 250.5 is the integer defect this migration
  -- exists to fix, so assert the fraction survived rather than only the value.
  IF lim = 250 OR lim = 251 THEN
    RAISE EXCEPTION 'the budget rounded to a whole cent: %', lim;
  END IF;
END
$$;

-- A budget for something that does not exist is an error, not a silent no-op.
DO $$
BEGIN
  PERFORM system_set_budget('bead', 'wg-test-no-such-bead', 100);
  RAISE EXCEPTION 'a budget was set for a bead the platform has never seen';
EXCEPTION WHEN raise_exception THEN
  IF SQLERRM NOT LIKE '%no work reference for bead%' THEN RAISE; END IF;
END
$$;

DO $$
BEGIN
  PERFORM system_set_budget('galaxy', 'wg-test-a', 100);
  RAISE EXCEPTION 'an unknown subject kind was accepted';
EXCEPTION WHEN raise_exception THEN
  IF SQLERRM NOT LIKE '%subject kind must be%' THEN RAISE; END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- The most specific budget wins, and spend is summed at that same level
-- ---------------------------------------------------------------------------
DO $$
DECLARE kind text; key text; lim numeric; spent numeric; over boolean;
BEGIN
  -- Spend on bead A, tagged with the bead.
  PERFORM system_record_usage('wg-budget-1', 'g', 'anthropic', 'claude-opus-5',
      1000, 100, 100.0, false, true, 'polecat', 'wg-test-budget-cell', 'sandbox', now(), 'wg-test-a');
  -- And on bead B, same project, same cell.
  PERFORM system_record_usage('wg-budget-2', 'g', 'anthropic', 'claude-opus-5',
      1000, 100, 300.0, false, true, 'polecat', 'wg-test-budget-cell', 'sandbox', now(), 'wg-test-b');

  -- A has its own budget of 250.5 and has spent 100 of it.
  SELECT subject_kind, subject_key, daily_cents, spent_cents, exceeded
    INTO kind, key, lim, spent, over
    FROM system_budget_status('wg-test-a', 'wg-test-budget-cell');
  IF kind <> 'bead' OR key <> 'wg-test-a' THEN
    RAISE EXCEPTION 'the bead budget did not win: %/%', kind, key;
  END IF;
  IF spent <> 100 THEN
    RAISE EXCEPTION 'bead A was charged % cents, expected 100 — the other bead''s spend leaked in', spent;
  END IF;
  IF over THEN RAISE EXCEPTION 'bead A reported exceeded at 100 of 250.5'; END IF;

  -- B has no budget of its own, so its PROJECT's applies — and the project's
  -- spend is everything in it, both beads, 400.
  SELECT subject_kind, subject_key, daily_cents, spent_cents
    INTO kind, key, lim, spent
    FROM system_budget_status('wg-test-b', 'wg-test-budget-cell');
  IF kind <> 'project' OR key <> 'wg-test-budget-project' THEN
    RAISE EXCEPTION 'bead B fell through to %/%, expected the project', kind, key;
  END IF;
  IF spent <> 400 THEN
    RAISE EXCEPTION 'the project was charged % cents, expected 400 (both beads)', spent;
  END IF;
  IF lim <> 5000 THEN RAISE EXCEPTION 'the project limit came out as %', lim; END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- Exceeding it
-- ---------------------------------------------------------------------------
DO $$
DECLARE spent numeric; remaining numeric; over boolean;
BEGIN
  PERFORM system_record_usage('wg-budget-3', 'g', 'anthropic', 'claude-opus-5',
      1000, 100, 200.0, false, true, 'polecat', 'wg-test-budget-cell', 'sandbox', now(), 'wg-test-a');

  SELECT spent_cents, remaining_cents, exceeded INTO spent, remaining, over
    FROM system_budget_status('wg-test-a', 'wg-test-budget-cell');
  IF spent <> 300 THEN RAISE EXCEPTION 'bead A spend came out as %', spent; END IF;
  IF NOT over THEN
    RAISE EXCEPTION '300 cents against a 250.5 ceiling did not report exceeded';
  END IF;
  IF remaining >= 0 THEN
    RAISE EXCEPTION 'remaining came out as % when the budget is overspent', remaining;
  END IF;
END
$$;

-- Yesterday's spend must not count against today's ceiling, or a budget can
-- never recover and every bead is permanently blocked after one busy day.
DO $$
DECLARE spent numeric;
BEGIN
  PERFORM system_record_usage('wg-budget-old', 'g', 'anthropic', 'claude-opus-5',
      1000, 100, 9999.0, false, true, 'polecat', 'wg-test-budget-cell', 'sandbox',
      now() - interval '2 days', 'wg-test-a');
  SELECT spent_cents INTO spent FROM system_budget_status('wg-test-a', 'wg-test-budget-cell');
  IF spent <> 300 THEN
    RAISE EXCEPTION 'spend from two days ago counted against today: %', spent;
  END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- Staleness measures the IMPORT, not the data
-- ---------------------------------------------------------------------------
--
-- The distinction is the whole point. A gateway nobody has used for a week has a
-- newest record a week old however punctually the importer ran, and measuring
-- the record would refuse every dispatch in a quiet environment while the
-- importer was working perfectly. Observed on staging before it was fixed.
DO $$
DECLARE stale bigint; newest timestamptz;
BEGIN
  -- No import has been recorded yet, which is NOT freshness: NULL says "this
  -- cannot be judged" and the decision layer refuses on it.
  SELECT staleness_seconds INTO stale
    FROM system_budget_status('wg-test-a', 'wg-test-budget-cell');
  IF stale IS NOT NULL THEN
    RAISE EXCEPTION 'staleness was % with no import ever recorded, expected NULL', stale;
  END IF;

  PERFORM system_record_import_run('g', 400, true);

  SELECT staleness_seconds, newest_record_at INTO stale, newest
    FROM system_budget_status('wg-test-a', 'wg-test-budget-cell');
  IF stale IS NULL THEN
    RAISE EXCEPTION 'staleness came back NULL after an import was recorded';
  END IF;
  IF stale > 300 THEN
    RAISE EXCEPTION 'an import recorded seconds ago reported % seconds of staleness', stale;
  END IF;

  -- And the decisive case: the two must be INDEPENDENT. Move the recorded import
  -- back by an hour without touching a single usage record; staleness must
  -- follow the import and newest_record_at must not move.
  UPDATE usage_import_runs SET ran_at = now() - interval '1 hour' WHERE gateway = 'g';

  SELECT staleness_seconds, newest_record_at INTO stale, newest
    FROM system_budget_status('wg-test-a', 'wg-test-budget-cell');
  IF stale < 3500 OR stale > 3700 THEN
    RAISE EXCEPTION 'an import an hour old reported % seconds of staleness', stale;
  END IF;
  IF newest < now() - interval '1 minute' THEN
    RAISE EXCEPTION 'newest_record_at moved when only the import time changed: %', newest;
  END IF;

  UPDATE usage_import_runs SET ran_at = now() WHERE gateway = 'g';
END
$$;

-- The least recent gateway governs: one gateway unread for a day makes the total
-- wrong however promptly the others were read.
DO $$
DECLARE stale bigint;
BEGIN
  INSERT INTO usage_import_runs (gateway, ran_at) VALUES ('g-behind', now() - interval '6 hours');
  SELECT staleness_seconds INTO stale
    FROM system_budget_status('wg-test-a', 'wg-test-budget-cell');
  IF stale < 21000 THEN
    RAISE EXCEPTION 'a gateway six hours behind reported only % seconds of staleness', stale;
  END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- The operational limits come back with the ceiling (wg-726)
-- ---------------------------------------------------------------------------
--
-- max_concurrent_agents and max_runtime_minutes were stored from 0006 and read
-- by nothing. They are returned from the SAME row as the money, so a caller
-- deciding whether to dispatch gets one answer about one subject rather than
-- resolving twice and possibly landing on different budgets.
DO $$
DECLARE kind text; agents integer; runtime integer;
BEGIN
  PERFORM system_set_budget('bead', 'wg-test-a', 250.5, 3, 45);

  SELECT subject_kind, max_agents, max_runtime_minutes
    INTO kind, agents, runtime
    FROM system_budget_status('wg-test-a', 'wg-test-budget-cell');

  IF kind <> 'bead' THEN RAISE EXCEPTION 'resolved to % not bead', kind; END IF;
  IF agents <> 3 THEN RAISE EXCEPTION 'max_agents came back as %', agents; END IF;
  IF runtime <> 45 THEN RAISE EXCEPTION 'max_runtime_minutes came back as %', runtime; END IF;

  -- And a subject with no budget reports no limits rather than zero, which a
  -- caller would read as "no agents may run".
  SELECT max_agents, max_runtime_minutes INTO agents, runtime
    FROM system_budget_status('wg-test-no-such-bead', NULL);
  IF agents IS NOT NULL OR runtime IS NOT NULL THEN
    RAISE EXCEPTION 'limits invented for an unbudgeted bead: % / %', agents, runtime;
  END IF;
END
$$;

ROLLBACK;
