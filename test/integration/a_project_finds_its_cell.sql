-- A project created without naming a cell finds the shared one, and nothing is
-- guessed when that is ambiguous.
--
-- Which cells EXIST is infrastructure: a cell is a Linux user with cgroup
-- limits and a credential profile (ADR-0002), so creating one means touching
-- the machine. Which PROJECT runs in which cell is data -- it changes when
-- somebody starts a project.
--
-- Those were conflated in a project-to-cell table in group_vars, which put
-- client names in the repository and made starting a project an Ansible run. A
-- project created through the API or MCP got NO cell, so dispatch found no rig
-- and the work sat there: eight beads were filed that way and could not be
-- dispatched.
--
-- Owner-only by design, recorded in scripts/check_rls_tests.py: it asserts what
-- the registration functions wrote and what the default resolves to. Every
-- write goes through a SECURITY DEFINER function that runs with no app user,
-- and the reads verify those writes rather than who may see them.
\set ON_ERROR_STOP on

BEGIN;

DO $$
DECLARE node uuid; got text;
BEGIN
  INSERT INTO execution_nodes (hostname, environment)
  VALUES ('cellfinder-probe.invalid', 'staging')
  ON CONFLICT (hostname) DO UPDATE SET environment = EXCLUDED.environment
  RETURNING id INTO node;

  -- A client cell is not shared: its whole purpose is that one engagement
  -- cannot read another, so it must never absorb a project nobody placed.
  PERFORM system_register_execution_cell(
    'cellfinder-probe.invalid', 'cf-client', 'wgcell_cf_client', 'cf-client',
    2, 200, 8192, false);

  SELECT system_default_cell() INTO got;
  IF got <> '' THEN
      RAISE EXCEPTION 'with no shared cell the default resolved to %, so a project '
          'would have been placed in a trust domain nobody chose', quote_literal(got);
  END IF;

  -- One shared cell: that is the answer.
  PERFORM system_register_execution_cell(
    'cellfinder-probe.invalid', 'cf-shared', 'wgcell_cf_shared', 'cf-shared',
    2, 200, 8192, true);

  SELECT system_default_cell() INTO got;
  IF got <> 'cf-shared' THEN
      RAISE EXCEPTION 'the default resolved to %, not the one shared cell', quote_literal(got);
  END IF;

  -- Registering the client cell again must not make it shared by accident:
  -- the upsert carries the flag, so a re-register is where it could flip.
  PERFORM system_register_execution_cell(
    'cellfinder-probe.invalid', 'cf-client', 'wgcell_cf_client', 'cf-client',
    2, 200, 8192, false);
  SELECT system_default_cell() INTO got;
  IF got <> 'cf-shared' THEN
      RAISE EXCEPTION 're-registering a client cell changed the default to %', quote_literal(got);
  END IF;

  -- Two shared cells is ambiguous, and ambiguity is answered by asking. A
  -- project quietly placed in the wrong trust domain is the failure ADR-0002
  -- exists to prevent, so this returns nothing and the caller must name one.
  PERFORM system_register_execution_cell(
    'cellfinder-probe.invalid', 'cf-shared-2', 'wgcell_cf_shared_2', 'cf-shared-2',
    2, 200, 8192, true);

  SELECT system_default_cell() INTO got;
  IF got <> '' THEN
      RAISE EXCEPTION 'with two shared cells the default picked %; it must refuse to '
          'guess which trust domain a project belongs to', quote_literal(got);
  END IF;
END
$$;

ROLLBACK;

SELECT 'a project finds the shared cell, and nothing is guessed when that is ambiguous' AS result;
