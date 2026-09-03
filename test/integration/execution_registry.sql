-- Registering execution nodes and cells, and refusing to guess (wg-38w).
--
-- execution_nodes and execution_cells were both EMPTY on staging while the oss
-- cell was running and had produced 9,200 agent health events. Nothing had ever
-- written them, so three things were quietly broken: cost could not be
-- attributed to a project, agent_health_events.cell referred to nothing, and
-- nged could not be tightened from confidential to restricted because the schema
-- refuses a restricted project with no cell.
--
-- Everything here runs against the pilot seed, so it asserts the registry the
-- deployment will actually produce rather than a synthetic one.

BEGIN;

-- ---------------------------------------------------------------------------
-- Registration, and re-registration.
-- ---------------------------------------------------------------------------
DO $$
DECLARE changed boolean; n integer;
BEGIN
  changed := system_register_execution_node('wg-test-execution', 'staging');
  IF NOT changed THEN RAISE EXCEPTION 'registering a new node reported no change'; END IF;

  -- Idempotent on the hostname, which is its natural key. A deployment runs many
  -- times, must not accumulate a node per run, and must be able to SAY it
  -- changed nothing: the WP-B2 acceptance criterion is that a second Ansible run
  -- reports no changes, and a function that always reports one would break it
  -- while looking like it worked.
  changed := system_register_execution_node('wg-test-execution', 'staging');
  IF changed THEN RAISE EXCEPTION 'registering an unchanged node reported a change'; END IF;
  SELECT count(*) INTO n FROM execution_nodes WHERE hostname = 'wg-test-execution';
  IF n <> 1 THEN RAISE EXCEPTION 'registering the same node twice produced % rows', n; END IF;

  -- An environment change IS recorded rather than ignored: a registry that
  -- silently keeps the old value disagrees with the inventory, invisibly.
  changed := system_register_execution_node('wg-test-execution', 'production');
  IF NOT changed THEN RAISE EXCEPTION 'a node that moved environment reported no change'; END IF;
  SELECT count(*) INTO n FROM execution_nodes
   WHERE hostname = 'wg-test-execution' AND environment = 'production';
  IF n <> 1 THEN RAISE EXCEPTION 'a node that moved environment was not updated'; END IF;
  PERFORM system_register_execution_node('wg-test-execution', 'staging');

  changed := system_register_execution_cell(
    'wg-test-execution', 'wg-test-oss', 'wgcell_wg_test_oss', 'oss', 2, 88, 1398);
  IF NOT changed THEN RAISE EXCEPTION 'registering a new cell reported no change'; END IF;

  changed := system_register_execution_cell(
    'wg-test-execution', 'wg-test-oss', 'wgcell_wg_test_oss', 'oss', 2, 88, 1398);
  IF changed THEN RAISE EXCEPTION 'registering an unchanged cell reported a change'; END IF;

  -- Limits are re-registered as applied. WP-I3 derives them from the node's real
  -- CPU and memory, so a resized node must update the recorded ceiling rather
  -- than leaving the first run's numbers in place forever.
  changed := system_register_execution_cell(
    'wg-test-execution', 'wg-test-oss', 'wgcell_wg_test_oss', 'oss', 3, 175, 3072);
  IF NOT changed THEN RAISE EXCEPTION 'a cell whose limits were re-derived reported no change'; END IF;
  SELECT count(*) INTO n FROM execution_cells
   WHERE slug = 'wg-test-oss' AND cpu_quota_percent = 175 AND memory_limit_mb = 3072
     AND max_concurrent_agents = 3;
  IF n <> 1 THEN RAISE EXCEPTION 'a re-registered cell kept its old limits'; END IF;
END
$$;

-- A cell for an unknown node is refused rather than inventing the node. A node
-- conjured with no environment would hide the ordering mistake that produced it.
DO $$
BEGIN
  PERFORM system_register_execution_cell(
    'wg-test-nonexistent', 'wg-test-orphan', 'wgcell_orphan', 'oss', 2, 50, 512);
  RAISE EXCEPTION 'a cell was registered against a node that does not exist';
EXCEPTION WHEN raise_exception THEN
  IF SQLERRM NOT LIKE '%no execution node named%' THEN RAISE; END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- Attaching a project, and the tightening 0009 deferred.
-- ---------------------------------------------------------------------------
DO $$
DECLARE changed boolean; v text; c uuid;
BEGIN
  changed := system_register_execution_cell(
    'wg-test-execution', 'wg-test-client', 'wgcell_wg_test_client', 'client-nged', 2, 88, 1398);

  changed := system_attach_project_to_cell('portaljs-oss', 'wg-test-oss');
  IF NOT changed THEN RAISE EXCEPTION 'attaching a project reported no change'; END IF;

  -- Idempotent: a second deployment must report nothing to do rather than
  -- rewriting the row and bumping updated_at on every run.
  changed := system_attach_project_to_cell('portaljs-oss', 'wg-test-oss');
  IF changed THEN RAISE EXCEPTION 're-attaching the same project reported a change'; END IF;

  -- 0009 created nged as confidential ON PURPOSE: the schema refuses a
  -- restricted project with no execution_cell_id, and cells did not exist yet.
  -- This is the deferred half.
  SELECT visibility INTO v FROM projects WHERE slug = 'nged';
  IF v <> 'confidential' THEN
    RAISE EXCEPTION 'nged starts as %, so this test is asserting the wrong thing', v;
  END IF;

  changed := system_attach_project_to_cell('nged', 'wg-test-client', 'restricted');
  IF NOT changed THEN RAISE EXCEPTION 'tightening nged reported no change'; END IF;

  SELECT visibility, execution_cell_id INTO v, c FROM projects WHERE slug = 'nged';
  IF v <> 'restricted' OR c IS NULL THEN
    RAISE EXCEPTION 'nged is % with cell %, expected restricted with a cell', v, c;
  END IF;
END
$$;

-- A project that does not exist is an error, not a silent no-op. The no-op is
-- what a typo in group_vars would otherwise become.
DO $$
BEGIN
  PERFORM system_attach_project_to_cell('wg-test-no-such-project', 'wg-test-oss');
  RAISE EXCEPTION 'attaching a project that does not exist was accepted';
EXCEPTION WHEN raise_exception THEN
  IF SQLERRM NOT LIKE '%no project named%' THEN RAISE; END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- Attribution resolves when the cell is unambiguous, and refuses when it is not.
-- ---------------------------------------------------------------------------
--
-- A cell can host more than one project: docs/pilot/registry.md says
-- portaljs-oss "may share an execution cell with other non-sensitive work". The
-- first implementation resolved with LIMIT 1, which picks one of them at random
-- and hides the choice. That is the same mistake as attributing untagged spend
-- to a default, and a wrong attribution is much harder to notice than a missing
-- one.
DO $$
DECLARE was_new boolean; p uuid; recorded_cell text;
BEGIN
  was_new := system_record_usage('wg-test-usage-1', 'g', 'anthropic', 'claude-haiku-4-5',
                                 100, 10, 0.0088, false, true, 'polecat', 'wg-test-oss', 'sandbox',
                                 now());
  IF NOT was_new THEN RAISE EXCEPTION 'the usage row was not inserted'; END IF;

  SELECT project_id INTO p FROM usage_records WHERE external_id = 'wg-test-usage-1';
  IF p IS NULL OR p <> (SELECT id FROM projects WHERE slug = 'portaljs-oss') THEN
    RAISE EXCEPTION 'a cell hosting exactly one project did not resolve to it (got %)', p;
  END IF;

  -- Now share the cell.
  --
  -- datahub rather than datopian-products, which 0080 deleted when it dissolved
  -- the products project. Any second project makes the cell shared, which is
  -- the only property this needs.
  PERFORM system_attach_project_to_cell('datahub', 'wg-test-oss');

  PERFORM system_record_usage('wg-test-usage-2', 'g', 'anthropic', 'claude-haiku-4-5',
                              100, 10, 0.0088, false, true, 'polecat', 'wg-test-oss', 'sandbox',
                              now());

  SELECT project_id, cell INTO p, recorded_cell
    FROM usage_records WHERE external_id = 'wg-test-usage-2';
  IF p IS NOT NULL THEN
    RAISE EXCEPTION 'a cell hosting two projects attributed spend to one of them anyway (%)', p;
  END IF;
  IF recorded_cell <> 'wg-test-oss' THEN
    RAISE EXCEPTION 'the cell was not recorded, so where it ran is now unknowable too';
  END IF;
END
$$;

ROLLBACK;
