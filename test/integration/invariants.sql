-- Schema invariants that must hold regardless of application code.
-- Each block asserts that the database REFUSES something it must refuse.

-- Uses a suite-specific organisation slug so the suite runs against a database
-- that already carries the pilot seed, not only against an empty one. A test
-- that only passes on an empty database cannot check production-shaped state.
BEGIN;

INSERT INTO organisations (id, slug, name)
VALUES ('00000000-0000-0000-0000-0000000000a1', 'test-invariants', 'Test org: invariants');
INSERT INTO users (id, organisation_id, display_name) VALUES
  ('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000a1', 'Alice'),
  ('00000000-0000-0000-0000-0000000000b2', '00000000-0000-0000-0000-0000000000a1', 'Bob');

DO $$
DECLARE refused boolean;
BEGIN
  -- A project's backup owner must differ from its primary owner.
  refused := false;
  BEGIN
    INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id)
    VALUES ('00000000-0000-0000-0000-0000000000a1', 'p1', 'P1',
            '00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000b1');
  EXCEPTION WHEN check_violation THEN refused := true;
  END;
  IF NOT refused THEN RAISE EXCEPTION 'a project accepted the same primary and backup owner'; END IF;

  -- A restricted project must have its own execution cell.
  refused := false;
  BEGIN
    INSERT INTO projects (organisation_id, slug, name, visibility, primary_owner_id, backup_owner_id)
    VALUES ('00000000-0000-0000-0000-0000000000a1', 'p2', 'P2', 'restricted',
            '00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000b2');
  EXCEPTION WHEN check_violation THEN refused := true;
  END;
  IF NOT refused THEN RAISE EXCEPTION 'a restricted project was accepted without an execution cell'; END IF;

  -- The audit log is append-only.
  INSERT INTO audit_log (action, target_type, outcome) VALUES ('test.action', 'test', 'allowed');
  refused := false;
  BEGIN
    UPDATE audit_log SET outcome = 'denied' WHERE action = 'test.action';
  EXCEPTION WHEN others THEN refused := true;
  END;
  IF NOT refused THEN RAISE EXCEPTION 'the audit log accepted an UPDATE'; END IF;

  refused := false;
  BEGIN
    DELETE FROM audit_log WHERE action = 'test.action';
  EXCEPTION WHEN others THEN refused := true;
  END;
  IF NOT refused THEN RAISE EXCEPTION 'the audit log accepted a DELETE'; END IF;

  RAISE NOTICE 'core invariants: all assertions passed';
END
$$;

ROLLBACK;

-- Regression guard for the bug found in review: a permissive FOR ALL policy
-- also covers SELECT, so it re-opens every read it was meant to leave alone.
-- No policy on an RLS-protected table may use FOR ALL.
DO $$
DECLARE offending text;
BEGIN
  SELECT string_agg(format('%s.%s', schemaname, policyname), ', ')
    INTO offending
  FROM pg_policies
  WHERE schemaname = 'public' AND cmd = 'ALL';

  IF offending IS NOT NULL THEN
    RAISE EXCEPTION 'FOR ALL policies silently grant SELECT; scope these to INSERT/UPDATE/DELETE: %', offending;
  END IF;
  RAISE NOTICE 'no FOR ALL policies';
END
$$;
