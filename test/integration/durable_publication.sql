-- Accepting a candidate publishes it, or says why it did not (WP-H4).
--
-- The routing is not a new decision: knowledge_records.record_type is exactly
-- the durable-memory set, and the operational types belong in Beads. This
-- asserts that an accepted decision becomes a record with its provenance, and
-- that an accepted task does NOT quietly become nothing.

BEGIN;

DO $$
DECLARE org uuid; src uuid; proj uuid;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';
  SELECT id INTO proj FROM projects WHERE slug = 'cdt';
  PERFORM set_config('test.lead',
    (SELECT primary_owner_id::text FROM projects WHERE slug = 'cdt'), false);

  INSERT INTO knowledge_sources (organisation_id, provider, provider_source_id,
                                 provider_revision, source_type, project_id,
                                 visibility, captured_at)
  VALUES (org, 'google-meet', 'test/publication', 'sha256:pub', 'transcript',
          proj, 'restricted', now())
  RETURNING id INTO src;
  PERFORM set_config('test.source', src::text, false);

  INSERT INTO knowledge_candidates (source_id, candidate_type, statement, confidence,
                                    visibility, proposed_project_id, source_spans,
                                    extractor_prompt_version)
  VALUES (src, 'decision', 'Use batch ingestion for phase one', 0.94, 'restricted',
          proj, '[{"line":1,"text":"said it"}]'::jsonb, 'extract-test'),
         (src, 'task', 'Send the migration plan', 0.9, 'restricted',
          proj, '[{"line":2,"text":"said it"}]'::jsonb, 'extract-test'),
         -- One of every durable type. Only the decision was tested before, and
         -- it is the one type with no review date -- so the CHECK that a fact,
         -- a constraint and an assumption must name one went unexercised, and
         -- accepting any of those three raised a constraint violation instead
         -- of publishing.
         (src, 'fact', 'The staging control node has two cores', 0.9, 'restricted',
          proj, '[{"line":3,"text":"said it"}]'::jsonb, 'extract-test'),
         (src, 'constraint', 'CDT material stays in the CDT cell', 0.9, 'restricted',
          proj, '[{"line":4,"text":"said it"}]'::jsonb, 'extract-test'),
         (src, 'assumption', 'The pilot runs to the end of the quarter', 0.7, 'restricted',
          proj, '[{"line":5,"text":"said it"}]'::jsonb, 'extract-test'),
         (src, 'lesson', 'Verify a claim against the API before believing it', 0.9, 'restricted',
          proj, '[{"line":6,"text":"said it"}]'::jsonb, 'extract-test'),
         (src, 'preference', 'Write the reason a change exists in the commit', 0.9, 'restricted',
          proj, '[{"line":7,"text":"said it"}]'::jsonb, 'extract-test');
END $$;

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE n integer; res jsonb; rec uuid; cand uuid; kind text; horizon date;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.lead'), true);

  -- A durable type becomes a record.
  SELECT id INTO cand FROM knowledge_candidates
   WHERE source_id = current_setting('test.source')::uuid AND candidate_type = 'decision';
  res := review_candidate(cand, 'accept');
  IF res ->> 'published' <> 'knowledge_record' THEN
    RAISE EXCEPTION 'an accepted decision published as %', res ->> 'published';
  END IF;
  rec := (res ->> 'record_id')::uuid;

  SELECT count(*) INTO n FROM knowledge_records
   WHERE id = rec AND record_type = 'decision' AND scope = 'project'
     AND visibility = 'restricted'
     AND reviewer_user_id = current_setting('test.lead')::uuid
     AND human_authored = false;
  IF n <> 1 THEN
    RAISE EXCEPTION 'the published record does not carry the expected shape';
  END IF;

  -- Provenance, which the deferred trigger would otherwise refuse at commit.
  SELECT count(*) INTO n FROM knowledge_record_sources
   WHERE record_id = rec AND source_id = current_setting('test.source')::uuid
     AND candidate_id = cand;
  IF n <> 1 THEN
    RAISE EXCEPTION 'the record was published without naming its source';
  END IF;

  -- Visibility is inherited, never widened by acceptance. A reviewer accepting
  -- a restricted candidate must not produce an internal record.
  SELECT count(*) INTO n FROM knowledge_records
   WHERE id = rec AND visibility <> 'restricted';
  IF n <> 0 THEN
    RAISE EXCEPTION 'acceptance widened the classification';
  END IF;

  -- An operational type says it is queued for Beads, and the publisher (which
  -- runs bd on a host, not in here) is what turns that into a bead.
  SELECT id INTO cand FROM knowledge_candidates
   WHERE source_id = current_setting('test.source')::uuid AND candidate_type = 'task';
  res := review_candidate(cand, 'accept');
  IF res ->> 'published' <> 'queued_beads' THEN
    RAISE EXCEPTION 'an accepted task reported publication as %', res ->> 'published';
  END IF;
  IF (res -> 'record_id') <> 'null'::jsonb THEN
    RAISE EXCEPTION 'a task was written into durable memory, which is for the other types';
  END IF;

  -- Every durable type publishes, and the time-sensitive ones carry the
  -- horizon policies/knowledge-review.yaml declares.
  FOR cand IN
    SELECT id FROM knowledge_candidates
     WHERE source_id = current_setting('test.source')::uuid
       AND status = 'pending'
       AND candidate_type IN ('fact', 'constraint', 'assumption', 'lesson', 'preference')
     ORDER BY candidate_type
  LOOP
    SELECT candidate_type INTO kind FROM knowledge_candidates WHERE id = cand;
    res := review_candidate(cand, 'accept');
    IF res ->> 'published' <> 'knowledge_record' THEN
      RAISE EXCEPTION 'an accepted % published as %', kind, res ->> 'published';
    END IF;
    rec := (res ->> 'record_id')::uuid;

    SELECT review_after INTO horizon FROM knowledge_records WHERE id = rec;
    IF kind = 'fact' AND horizon IS DISTINCT FROM current_date + 180 THEN
      RAISE EXCEPTION 'a fact expires on % rather than in 180 days', horizon;
    END IF;
    IF kind = 'constraint' AND horizon IS DISTINCT FROM current_date + 365 THEN
      RAISE EXCEPTION 'a constraint expires on % rather than in 365 days', horizon;
    END IF;
    IF kind = 'assumption' AND horizon IS DISTINCT FROM current_date + 90 THEN
      RAISE EXCEPTION 'an assumption expires on % rather than in 90 days', horizon;
    END IF;
    -- A lesson and a preference are superseded, not expired. A review date on
    -- one would put it in the stale-record queue forever.
    IF kind IN ('lesson', 'preference') AND horizon IS NOT NULL THEN
      RAISE EXCEPTION 'a % was given a review date of %', kind, horizon;
    END IF;
  END LOOP;

  RAISE NOTICE 'durable publication assertions passed';
END $$;

ROLLBACK;
