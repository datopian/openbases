-- Who may decide on a candidate, and what a decision must carry (WP-H3).
--
-- Three tables gained row-level security in 0054 -- knowledge_reviews,
-- source_acl_entries and source_snapshots -- all of which had none. This asserts
-- the rules that now stand, including the one that matters most: a review
-- cannot be recorded under somebody else's name.

BEGIN;

DO $$
DECLARE org uuid; src uuid; cand uuid; proj uuid;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';
  SELECT id INTO proj FROM projects WHERE slug = 'cdt';

  PERFORM set_config('test.cdt_lead',
    (SELECT primary_owner_id::text FROM projects WHERE slug = 'cdt'), false);
  PERFORM set_config('test.nged_lead',
    (SELECT primary_owner_id::text FROM projects WHERE slug = 'nged'), false);

  INSERT INTO knowledge_sources (organisation_id, provider, provider_source_id,
                                 provider_revision, source_type, project_id,
                                 visibility, captured_at)
  VALUES (org, 'google-meet', 'test/transcripts/rev', 'sha256:test', 'transcript',
          proj, 'restricted', now())
  RETURNING id INTO src;

  INSERT INTO knowledge_candidates (source_id, candidate_type, statement,
                                    confidence, visibility, source_spans,
                                    extractor_prompt_version)
  VALUES (src, 'task', 'A candidate for the review test', 0.9, 'restricted',
          '[{"line":1,"text":"said something"}]'::jsonb, 'extract-test')
  RETURNING id INTO cand;

  PERFORM set_config('test.candidate', cand::text, false);
  PERFORM set_config('test.source', src::text, false);
END $$;

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
-- v_status, not `status`: knowledge_candidates has a column of that name and
-- plpgsql resolves the variable first, making every later reference ambiguous.
DECLARE n integer; v_status text; cand uuid := current_setting('test.candidate')::uuid;
BEGIN
  -- The other client's lead sees nothing of it. Two restricted client projects
  -- exist, so this is the leak that matters more than an outsider's.
  PERFORM set_config('workgraph.user_id', current_setting('test.nged_lead'), true);
  SELECT count(*) INTO n FROM knowledge_candidates WHERE id = cand;
  IF n <> 0 THEN RAISE EXCEPTION 'the nged lead can see a CDT candidate'; END IF;
  SELECT count(*) INTO n FROM knowledge_sources
   WHERE id = current_setting('test.source')::uuid;
  IF n <> 0 THEN RAISE EXCEPTION 'the nged lead can see a CDT source'; END IF;

  -- And cannot decide on it either. Refused by the policy rather than by
  -- whichever handler happens to be in front of it.
  BEGIN
    PERFORM review_candidate(cand, 'accept');
    RAISE EXCEPTION 'the nged lead decided on a CDT candidate';
  EXCEPTION WHEN raise_exception THEN
    IF SQLERRM LIKE '%decided on a CDT candidate%' THEN RAISE; END IF;
  END;

  -- The CDT lead can.
  PERFORM set_config('workgraph.user_id', current_setting('test.cdt_lead'), true);
  SELECT count(*) INTO n FROM knowledge_candidates WHERE id = cand;
  IF n <> 1 THEN RAISE EXCEPTION 'the cdt lead cannot see their own candidate'; END IF;

  -- A rejection without a reason is refused: the reason IS the evaluation data
  -- the improvement loop reads, so a rejection without one teaches nothing.
  BEGIN
    PERFORM review_candidate(cand, 'reject', NULL);
    RAISE EXCEPTION 'a rejection with no reason was accepted';
  EXCEPTION WHEN check_violation THEN
    NULL;
  END;

  -- An edit must carry the corrected statement, or the correction is lost.
  BEGIN
    PERFORM review_candidate(cand, 'edit_and_accept', NULL, NULL);
    RAISE EXCEPTION 'an edit with no statement was accepted';
  EXCEPTION WHEN check_violation THEN
    NULL;
  END;

  -- A real decision moves the candidate and records the reviewer in one step.
  SELECT review_candidate(cand, 'edit_and_accept', 'tightened the wording',
                          'A corrected statement') INTO v_status;
  IF v_status <> 'edited_accepted' THEN
    RAISE EXCEPTION 'status after an edit was %', v_status;
  END IF;

  SELECT count(*) INTO n FROM knowledge_candidates
   WHERE id = cand AND status = 'edited_accepted'
     AND statement = 'A corrected statement';
  IF n <> 1 THEN
    RAISE EXCEPTION 'the candidate did not take the edit';
  END IF;

  SELECT count(*) INTO n FROM knowledge_reviews
   WHERE candidate_id = cand
     AND reviewer_user_id = current_setting('test.cdt_lead')::uuid
     AND decision = 'edit_and_accept';
  IF n <> 1 THEN RAISE EXCEPTION 'the decision was not recorded against the reviewer'; END IF;

  -- Deciding twice is refused rather than silently overwriting. Two reviewers
  -- reaching the queue together is expected; the second being told is the
  -- difference between a race and a lost decision.
  BEGIN
    PERFORM review_candidate(cand, 'accept');
    RAISE EXCEPTION 'a decided candidate was decided again';
  EXCEPTION WHEN raise_exception THEN
    IF SQLERRM NOT LIKE '%is not pending%' THEN RAISE; END IF;
  END;

  RAISE NOTICE 'knowledge review assertions passed';
END $$;

-- A review recorded under somebody else's name would make the audit trail worse
-- than not having one, so the policy refuses it rather than trusting callers.
DO $$
DECLARE cand uuid := current_setting('test.candidate')::uuid;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.cdt_lead'), true);
  BEGIN
    INSERT INTO knowledge_reviews (candidate_id, reviewer_user_id, decision, reason)
    VALUES (cand, current_setting('test.nged_lead')::uuid, 'accept', 'not me');
    RAISE EXCEPTION 'a review was recorded under another user';
  EXCEPTION WHEN insufficient_privilege THEN
    NULL;  -- refused by the policy, as intended
  END;
  RAISE NOTICE 'reviewer identity assertions passed';
END $$;

ROLLBACK;
