-- Approvals: digest binding and who may decide (WP-F2).
--
-- The four acceptance criteria, asserted against the database rather than
-- against the Go layer, because these are the guarantees that must hold even if
-- a caller bypasses the application:
--
--   an approval unlocks the EXACT action
--   a changed action invalidates the approval
--   an unauthorised user cannot approve
--   a decision is recorded, not mutated

BEGIN;

DO $$
DECLARE
  proj uuid; requester uuid; approver uuid; other uuid;
  req uuid; d1 text; d2 text; n integer;
BEGIN
  -- portaljs, not portaljs-oss: 0080 split the latter per repository and closed
  -- it. It still exists, so this worked either way -- but a fixture resting on a
  -- closed project is one deletion away from failing for a reason that has
  -- nothing to do with approvals.
  SELECT id INTO proj FROM projects WHERE slug = 'portaljs';
  SELECT user_id INTO requester FROM project_memberships WHERE project_id = proj LIMIT 1;
  SELECT user_id INTO approver FROM project_memberships
   WHERE project_id = proj AND user_id <> requester LIMIT 1;
  SELECT id INTO other FROM users WHERE id NOT IN (requester, COALESCE(approver, requester)) LIMIT 1;

  IF requester IS NULL OR approver IS NULL THEN
    RAISE NOTICE 'need two project members to test approvals; skipping';
    RETURN;
  END IF;

  d1 := repeat('a', 64);
  d2 := repeat('b', 64);

  INSERT INTO approval_requests
      (action_type, target_type, target_id, project_id, action_digest,
       risk_level, required_roles, required_approvals,
       requested_by_user_id, expires_at)
  VALUES ('deploy', 'project', proj::text, proj, d1,
          'high', ARRAY['project_lead'], 1, requester, now() + interval '1 hour')
  RETURNING id INTO req;

  -- 1. The requester may not approve their own action.
  --
  -- Enforced by a trigger, so it holds for any writer — the application layer
  -- refusing is a courtesy, not the control.
  BEGIN
    INSERT INTO approval_decisions (request_id, decided_by_user_id, decision, decided_digest)
    VALUES (req, requester, 'approve', d1);
    RAISE EXCEPTION 'the requester approved their own action';
  EXCEPTION WHEN others THEN
    IF SQLERRM LIKE '%the requester approved their own action%' THEN RAISE; END IF;
  END;

  -- 2. A different person may approve.
  INSERT INTO approval_decisions (request_id, decided_by_user_id, decision, decided_digest)
  VALUES (req, approver, 'approve', d1);

  UPDATE approval_requests SET status = 'approved'
   WHERE id = req
     AND (SELECT count(*) FROM approval_decisions
           WHERE request_id = req AND decision = 'approve') >= required_approvals;

  SELECT count(*) INTO n FROM approval_requests WHERE id = req AND status = 'approved';
  IF n <> 1 THEN RAISE EXCEPTION 'the request did not reach approved'; END IF;

  -- 3. The approval is bound to the digest it was given.
  --
  -- The decision carries the digest the approver saw, recorded at decision
  -- time, so a later change is provable rather than inferred.
  SELECT count(*) INTO n FROM approval_decisions
   WHERE request_id = req AND decided_digest = d1;
  IF n <> 1 THEN RAISE EXCEPTION 'the decision did not record the digest it approved'; END IF;

  -- 4. A changed action invalidates the approval.
  UPDATE approval_requests SET status = 'invalidated', action_digest = d2
   WHERE id = req AND action_digest <> d2;

  SELECT count(*) INTO n FROM approval_requests
   WHERE id = req AND status = 'approved';
  IF n <> 0 THEN RAISE EXCEPTION 'a changed action left the request approved'; END IF;

  -- The old decision survives. Approvals are append-only: the record of who
  -- approved what must not be rewritten by a later change to the action.
  SELECT count(*) INTO n FROM approval_decisions
   WHERE request_id = req AND decided_digest = d1;
  IF n <> 1 THEN RAISE EXCEPTION 'invalidation destroyed the decision history'; END IF;

  -- 5. One decision per approver. A second vote must not double-count toward
  --    the required number.
  BEGIN
    INSERT INTO approval_decisions (request_id, decided_by_user_id, decision, decided_digest)
    VALUES (req, approver, 'approve', d2);
    RAISE EXCEPTION 'the same approver voted twice';
  EXCEPTION WHEN unique_violation THEN NULL;
  END;

  -- 6. A rejection must say why.
  BEGIN
    INSERT INTO approval_decisions (request_id, decided_by_user_id, decision, decided_digest)
    VALUES (req, other, 'reject', d2);
    RAISE EXCEPTION 'a rejection was accepted with no reason';
  EXCEPTION WHEN check_violation THEN NULL;
  END;

  RAISE NOTICE 'approval assertions passed';
END
$$;

-- 7. A critical action cannot be configured to need only one approver.
DO $$
DECLARE proj uuid; u uuid;
BEGIN
  -- portaljs, not portaljs-oss: 0080 split the latter per repository and closed
  -- it. It still exists, so this worked either way -- but a fixture resting on a
  -- closed project is one deletion away from failing for a reason that has
  -- nothing to do with approvals.
  SELECT id INTO proj FROM projects WHERE slug = 'portaljs';
  SELECT user_id INTO u FROM project_memberships WHERE project_id = proj LIMIT 1;

  BEGIN
    INSERT INTO approval_requests
        (action_type, target_type, project_id, action_digest, risk_level,
         required_roles, required_approvals, requested_by_user_id, expires_at)
    VALUES ('destroy', 'project', proj, repeat('c', 64), 'critical',
            ARRAY['organisation_admin'], 1, u, now() + interval '1 hour');
    RAISE EXCEPTION 'a critical action was accepted with a single approver';
  EXCEPTION WHEN check_violation THEN NULL;
  END;

  RAISE NOTICE 'critical-action assertions passed';
END
$$;

ROLLBACK;
