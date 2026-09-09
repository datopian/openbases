-- A running job can say it is alive; a finished one cannot.
--
-- The gap this fills: between claim and result the control plane heard nothing,
-- so a two-hour run and a wedged one were the same picture and the only way to
-- tell was to ssh to the node.
--
-- The rule that matters is the negative one. A heartbeat recorded against a job
-- that has already finished would make a completed run look live, and a stale
-- heartbeat is precisely what tells somebody a run is stuck -- so a heartbeat
-- that can land on a finished row destroys the signal it exists to provide.
--
-- Owner-only by design, recorded in scripts/check_rls_tests.py: every write is
-- through a SECURITY DEFINER function the node calls with no app user, and the
-- assertions are about which rows it touched.
\set ON_ERROR_STOP on

BEGIN;

DO $$
DECLARE
  org uuid; node uuid; cell uuid; u uuid; running uuid; finished uuid;
  live boolean; beat timestamptz; note text;
BEGIN
  SELECT id INTO org FROM organisations ORDER BY created_at LIMIT 1;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Beat Requester', 'beat@example.invalid') RETURNING id INTO u;

  INSERT INTO execution_nodes (hostname, environment)
  VALUES ('beat-probe.invalid','staging')
  ON CONFLICT (hostname) DO UPDATE SET environment = EXCLUDED.environment RETURNING id INTO node;
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  VALUES (node,'beat-cell','wgcell_beat','oss') RETURNING id INTO cell;

  INSERT INTO work_queue (kind, cell, rig, bead, status, requested_by, claimed_at)
  VALUES ('work','beat-cell','r1','bt-1','running', u, now() - interval '40 minutes')
  RETURNING id INTO running;
  INSERT INTO work_queue (kind, cell, rig, bead, status, requested_by, claimed_at, finished_at)
  VALUES ('work','beat-cell','r1','bt-2','done', u, now() - interval '2 hours',
          now() - interval '1 hour')
  RETURNING id INTO finished;

  -- A running job records, and says it did.
  SELECT system_record_heartbeat(running, '12 KB of output, last wrote 3s ago') INTO live;
  IF NOT live THEN
      RAISE EXCEPTION 'a running job reported not live, so the node would stop reporting on it';
  END IF;
  SELECT heartbeat_at, heartbeat_note INTO beat, note FROM work_queue WHERE id = running;
  IF beat IS NULL THEN
      RAISE EXCEPTION 'the heartbeat was not recorded';
  END IF;
  IF note <> '12 KB of output, last wrote 3s ago' THEN
      RAISE EXCEPTION 'the note reads as %', quote_nullable(note);
  END IF;

  -- THE RULE. A finished job records nothing and says so, so a completed run
  -- can never look live.
  SELECT system_record_heartbeat(finished, 'still going!') INTO live;
  IF live THEN
      RAISE EXCEPTION 'a finished job accepted a heartbeat and reported itself live';
  END IF;
  SELECT heartbeat_at INTO beat FROM work_queue WHERE id = finished;
  IF beat IS NOT NULL THEN
      RAISE EXCEPTION 'a heartbeat landed on a finished job, so it now looks live';
  END IF;

  -- A job nobody has heard of is not an error: the node may be reporting on
  -- something already deleted, and raising would fail a pass that is otherwise
  -- doing its work.
  SELECT system_record_heartbeat(gen_random_uuid(), 'x') INTO live;
  IF live THEN
      RAISE EXCEPTION 'a heartbeat for an unknown job reported itself live';
  END IF;

  -- A long note is truncated rather than refused. It is a diagnostic, and
  -- losing a run's liveness signal to protect a column width would be the
  -- wrong trade.
  SELECT system_record_heartbeat(running, repeat('x', 500)) INTO live;
  SELECT heartbeat_note INTO note FROM work_queue WHERE id = running;
  IF length(note) > 200 THEN
      RAISE EXCEPTION 'the note was stored at % characters', length(note);
  END IF;
END
$$;

ROLLBACK;

SELECT 'a running job reports itself alive and a finished one cannot' AS result;
