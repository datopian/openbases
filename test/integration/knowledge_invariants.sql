-- Knowledge-layer invariants: provenance, freshness, supersession, approval.

-- Uses a suite-specific organisation slug so the suite runs against a database
-- that already carries the pilot seed, not only against an empty one. A test
-- that only passes on an empty database cannot check production-shaped state.
BEGIN;

INSERT INTO organisations (id, slug, name)
VALUES ('00000000-0000-0000-0000-0000000000a1', 'test-knowledge', 'Test org: knowledge_invariants');
INSERT INTO users (id, organisation_id, display_name) VALUES
  ('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000a1', 'Alice'),
  ('00000000-0000-0000-0000-0000000000b2', '00000000-0000-0000-0000-0000000000a1', 'Bob');

DO $$
DECLARE refused boolean; rid uuid;
BEGIN
  -- A record with neither a source nor a named author has no provenance.
  refused := false;
  BEGIN
    INSERT INTO knowledge_records (organisation_id, record_type, statement, scope,
                                   owner_user_id, reviewer_user_id, visibility, review_after)
    VALUES ('00000000-0000-0000-0000-0000000000a1', 'fact', 'Unsourced claim.', 'company',
            '00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000b1',
            'internal', CURRENT_DATE + 90);
    -- The provenance trigger is deferred, so it fires here.
    SET CONSTRAINTS knowledge_record_provenance IMMEDIATE;
  EXCEPTION WHEN others THEN refused := true;
  END;
  IF NOT refused THEN RAISE EXCEPTION 'a knowledge record was accepted with no provenance'; END IF;
END
$$;

ROLLBACK;
BEGIN;

INSERT INTO organisations (id, slug, name)
VALUES ('00000000-0000-0000-0000-0000000000a1', 'test-knowledge', 'Test org: knowledge_invariants');
INSERT INTO users (id, organisation_id, display_name) VALUES
  ('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000a1', 'Alice'),
  ('00000000-0000-0000-0000-0000000000b2', '00000000-0000-0000-0000-0000000000a1', 'Bob');

DO $$
DECLARE refused boolean;
BEGIN
  -- A time-sensitive record must declare when it goes stale.
  refused := false;
  BEGIN
    INSERT INTO knowledge_records (organisation_id, record_type, statement, scope,
                                   owner_user_id, reviewer_user_id, visibility,
                                   human_authored, author_user_id)
    VALUES ('00000000-0000-0000-0000-0000000000a1', 'fact', 'A fact with no expiry.', 'company',
            '00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000b1',
            'internal', true, '00000000-0000-0000-0000-0000000000b1');
  EXCEPTION WHEN check_violation THEN refused := true;
  END;
  IF NOT refused THEN RAISE EXCEPTION 'a fact was accepted without a review_after date'; END IF;

  -- A superseded record must name its successor.
  refused := false;
  BEGIN
    INSERT INTO knowledge_records (organisation_id, record_type, statement, scope,
                                   owner_user_id, reviewer_user_id, visibility, status,
                                   human_authored, author_user_id)
    VALUES ('00000000-0000-0000-0000-0000000000a1', 'decision', 'Old decision.', 'company',
            '00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000b1',
            'internal', 'superseded', true, '00000000-0000-0000-0000-0000000000b1');
  EXCEPTION WHEN check_violation THEN refused := true;
  END;
  IF NOT refused THEN RAISE EXCEPTION 'a superseded record was accepted without a successor'; END IF;

  -- Nothing approves itself.
  refused := false;
  BEGIN
    INSERT INTO improvement_proposals (target, problem_statement, expected_improvement,
                                       rollback_plan, proposed_by_user_id, approved_by_user_id, status)
    VALUES ('prompt', 'Extraction misses commitments.', 'Higher recall.', 'Revert the prompt version.',
            '00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000b1', 'approved');
  EXCEPTION WHEN check_violation THEN refused := true;
  END;
  IF NOT refused THEN RAISE EXCEPTION 'a proposal was approved by its own author'; END IF;

  -- An approver may not approve their own request.
  INSERT INTO approval_requests (id, action_type, target_type, action_digest, risk_level,
                                 required_roles, requested_by_user_id, expires_at)
  VALUES ('00000000-0000-0000-0000-0000000000e9', 'deployment.production', 'release',
          repeat('a', 64), 'high', ARRAY['project_lead'],
          '00000000-0000-0000-0000-0000000000b1', now() + interval '1 day');

  refused := false;
  BEGIN
    INSERT INTO approval_decisions (request_id, decided_by_user_id, decision, decided_digest)
    VALUES ('00000000-0000-0000-0000-0000000000e9', '00000000-0000-0000-0000-0000000000b1',
            'approve', repeat('a', 64));
  EXCEPTION WHEN others THEN refused := true;
  END;
  IF NOT refused THEN RAISE EXCEPTION 'a user approved their own request'; END IF;

  -- A different human may.
  INSERT INTO approval_decisions (request_id, decided_by_user_id, decision, decided_digest)
  VALUES ('00000000-0000-0000-0000-0000000000e9', '00000000-0000-0000-0000-0000000000b2',
          'approve', repeat('a', 64));

  -- A credential reference must not hold a credential value.
  refused := false;
  BEGIN
    INSERT INTO credential_refs (name, credential_class, storage_location)
    VALUES ('leak', 'agent_provider', 'sk-' || repeat('x', 40));
  EXCEPTION WHEN check_violation THEN refused := true;
  END;
  IF NOT refused THEN RAISE EXCEPTION 'a credential value was accepted as a storage location'; END IF;

  RAISE NOTICE 'knowledge and approval invariants: all assertions passed';
END
$$;

ROLLBACK;
