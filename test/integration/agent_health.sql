-- Agent health: the escalation path the deterministic witness depends on.
--
-- These assert the guarantees at the database level, because the witness runs
-- on an execution node with no user identity and reaches the inbox only through
-- these functions. If the functions are wrong, no amount of care in the Go
-- layer helps:
--
--   a standing problem produces ONE inbox item, not one per pass
--   a different problem with the same polecat raises its own item
--   an operator's snooze survives the machine seeing the problem again
--   an escalation with no possible recipient is reported as reaching nobody
--   every decision is recorded, observations included

BEGIN;

DO $$
DECLARE
  proj uuid; slug text; lead uuid; n integer; before_count integer; after_count integer;
  expl jsonb := '{"source":"witness","reason":"git-dirty"}'::jsonb;
BEGIN
  SELECT p.id, p.slug INTO proj, slug FROM projects p
    JOIN project_memberships m ON m.project_id = p.id AND m.role_name = 'project_lead'
   LIMIT 1;

  IF proj IS NULL THEN
    -- Said out loud. A vacuous pass on an unseeded database is the failure mode
    -- that let an earlier envelope test report success while asserting nothing.
    RAISE NOTICE 'SKIP: no project with a project_lead; agent health path not exercised';
    RETURN;
  END IF;

  SELECT user_id INTO lead FROM system_attention_recipients(slug) LIMIT 1;
  IF lead IS NULL THEN
    RAISE EXCEPTION 'a project with a project_lead must have a recipient';
  END IF;

  -- ---------------------------------------------------------------------
  -- Recording, including the quiet decisions
  -- ---------------------------------------------------------------------
  PERFORM system_record_agent_health('oss', 'sandbox', 'nitro', 'observe',
      'working', 'session_running=true', NULL, false);
  PERFORM system_record_agent_health('oss', 'sandbox', 'dust', 'escalate',
      'git-dirty', 'session_running=false, verdict=NEEDS_RECOVERY', 'sa-03r', false);

  SELECT count(*) INTO n FROM agent_health_events
   WHERE cell = 'oss' AND rig = 'sandbox' AND polecat IN ('nitro', 'dust');
  IF n <> 2 THEN
    RAISE EXCEPTION 'expected both decisions recorded, got %', n;
  END IF;

  IF EXISTS (SELECT 1 FROM agent_health_events WHERE used_model) THEN
    RAISE EXCEPTION 'the witness must settle these cases without a model';
  END IF;

  -- ---------------------------------------------------------------------
  -- One item per situation, however often it is observed
  -- ---------------------------------------------------------------------
  SELECT count(*) INTO before_count FROM attention_items WHERE user_id = lead;

  SELECT system_raise_attention(slug, 'agent_work_at_risk',
      'witness:sandbox/dust:git-dirty', 0.9, expl, 'internal') INTO n;
  IF n < 1 THEN
    RAISE EXCEPTION 'the first escalation reached nobody';
  END IF;

  -- The same situation, seen again on the next pass.
  PERFORM system_raise_attention(slug, 'agent_work_at_risk',
      'witness:sandbox/dust:git-dirty', 0.9,
      expl || '{"silence":"20m"}'::jsonb, 'internal');
  PERFORM system_raise_attention(slug, 'agent_work_at_risk',
      'witness:sandbox/dust:git-dirty', 0.9, expl, 'internal');

  SELECT count(*) INTO after_count FROM attention_items
   WHERE user_id = lead AND dedupe_key = 'witness:sandbox/dust:git-dirty';
  IF after_count <> 1 THEN
    RAISE EXCEPTION 'a standing problem produced % inbox items; it must produce one', after_count;
  END IF;

  -- The explanation is refreshed even though no new row appeared, because the
  -- situation moves on and a stale basis is worse than none.
  IF NOT EXISTS (
      SELECT 1 FROM attention_items
       WHERE user_id = lead AND dedupe_key = 'witness:sandbox/dust:git-dirty'
         AND last_seen_at IS NOT NULL) THEN
    RAISE EXCEPTION 'the repeated observation did not refresh last_seen_at';
  END IF;

  -- ---------------------------------------------------------------------
  -- A different problem is a different item
  -- ---------------------------------------------------------------------
  PERFORM system_raise_attention(slug, 'agent_silent',
      'witness:sandbox/dust:silent-session', 0.5, expl, 'internal');

  SELECT count(*) INTO n FROM attention_items
   WHERE user_id = lead AND dedupe_key LIKE 'witness:sandbox/dust:%';
  IF n <> 2 THEN
    RAISE EXCEPTION 'a second, different problem should raise its own item; got % total', n;
  END IF;

  -- ---------------------------------------------------------------------
  -- A snooze is the operator's decision and the machine must not undo it
  -- ---------------------------------------------------------------------
  UPDATE attention_items
     SET status = 'snoozed', snoozed_until = now() + interval '1 day'
   WHERE user_id = lead AND dedupe_key = 'witness:sandbox/dust:git-dirty';

  PERFORM system_raise_attention(slug, 'agent_work_at_risk',
      'witness:sandbox/dust:git-dirty', 0.9, expl, 'internal');

  IF NOT EXISTS (
      SELECT 1 FROM attention_items
       WHERE user_id = lead AND dedupe_key = 'witness:sandbox/dust:git-dirty'
         AND status = 'snoozed') THEN
    RAISE EXCEPTION 'seeing the problem again un-snoozed an item the operator had deferred';
  END IF;

  SELECT count(*) INTO n FROM attention_items
   WHERE user_id = lead AND dedupe_key = 'witness:sandbox/dust:git-dirty';
  IF n <> 1 THEN
    RAISE EXCEPTION 'a snoozed item was duplicated rather than updated; got %', n;
  END IF;

  -- ---------------------------------------------------------------------
  -- Nobody to tell is an answer, not an error
  -- ---------------------------------------------------------------------
  SELECT system_raise_attention('a-project-that-does-not-exist', 'agent_work_at_risk',
      'witness:nowhere/none:git-dirty', 0.9, expl, 'internal') INTO n;
  -- Zero recipients is the signal the caller acts on. It must not raise, and it
  -- must not quietly file the item against nobody.
  IF n <> 0 THEN
    RAISE EXCEPTION 'an unknown project should reach nobody, reported %', n;
  END IF;

  RAISE NOTICE 'agent health escalation path OK (recipient %)', lead;
END $$;

ROLLBACK;
