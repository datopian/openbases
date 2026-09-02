-- A reviewer can correct a candidate, not only decide on it (WP-H3).
--
-- WP-H3 asks for a reviewer who can "accept, edit, reject, defer, reclassify
-- and assign owner or project". Without the last three the queue's only answer
-- to "this is nearly right" is to throw it away -- and the transcript has
-- already been ingested at that revision, so it never comes back.
--
-- The limits are the interesting part: tightening a classification is allowed
-- and widening is not, and a project can only be assigned by somebody who can
-- read it.

BEGIN;

DO $$
DECLARE org uuid; src uuid; reviewer uuid; outsider uuid; alpha uuid; beta uuid; cell uuid;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';
  SELECT primary_owner_id INTO reviewer FROM projects WHERE slug = 'cdt';
  PERFORM set_config('test.reviewer', reviewer::text, false);

  -- Separate inserts: a multi-row INSERT ... RETURNING INTO a scalar raises
  -- "query returned more than one row", which reads like a lookup problem
  -- rather than like two rows going into one variable.
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Corrections outsider', 'test-corr-outsider@example.invalid')
  RETURNING id INTO outsider;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Corrections owner', 'test-corr-owner@example.invalid');
  PERFORM set_config('test.outsider', outsider::text, false);

  INSERT INTO execution_nodes (hostname, environment) VALUES ('test-corr-node', 'staging');
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  SELECT id, 'test-corr-cell', 'wgcell_test_corr', 'client-restricted'
    FROM execution_nodes WHERE hostname = 'test-corr-node'
  RETURNING id INTO cell;

  -- alpha the reviewer is a member of, beta they are not: assigning to beta
  -- must be refused even though beta exists.
  INSERT INTO projects (organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id, execution_cell_id)
  VALUES (org, 'test-corr-alpha', 'Corrections alpha', 'confidential',
          reviewer, outsider, NULL)
  RETURNING id INTO alpha;
  INSERT INTO projects (organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id, execution_cell_id)
  VALUES (org, 'test-corr-beta', 'Corrections beta', 'restricted',
          outsider,
          (SELECT id FROM users WHERE primary_email = 'test-corr-owner@example.invalid'), cell)
  RETURNING id INTO beta;

  INSERT INTO project_memberships (project_id, user_id, role_name)
  VALUES (alpha, reviewer, 'contributor');

  PERFORM set_config('test.alpha', alpha::text, false);
  PERFORM set_config('test.beta', beta::text, false);

  INSERT INTO knowledge_sources (organisation_id, provider, provider_source_id,
                                 provider_revision, source_type, project_id,
                                 visibility, captured_at)
  VALUES (org, 'google-meet', 'test/corrections', 'sha256:corr', 'transcript',
          alpha, 'confidential', now())
  RETURNING id INTO src;
  PERFORM set_config('test.source', src::text, false);

  -- The extractor's guesses: a task it called a question, with nobody's name
  -- on it and no project.
  INSERT INTO knowledge_candidates (source_id, candidate_type, statement, confidence,
                                    visibility, source_spans, extractor_prompt_version)
  VALUES (src, 'question', 'Send alpha the revised scope by Friday', 0.6,
          'confidential', '[{"line":1,"text":"said it"}]'::jsonb, 'extract-test'),
         (src, 'fact', 'The pilot has two cores', 0.8,
          'confidential', '[{"line":2,"text":"said it"}]'::jsonb, 'extract-test'),
         (src, 'decision', 'Widening is refused', 0.9,
          'restricted', '[{"line":3,"text":"said it"}]'::jsonb, 'extract-test'),
         (src, 'task', 'Assigning to an unreadable project is refused', 0.9,
          'confidential', '[{"line":4,"text":"said it"}]'::jsonb, 'extract-test');
END $$;

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE cand uuid; res jsonb; row_out record; caught text; n integer;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.reviewer'), true);

  -- Reclassify, assign an owner and a project, all in the act of accepting.
  SELECT id INTO cand FROM knowledge_candidates
   WHERE statement = 'Send alpha the revised scope by Friday';
  res := review_candidate(cand, 'accept', 'the extractor guessed the type',
                          NULL, 'task', 'test-corr-owner@example.invalid', 'test-corr-alpha');

  IF res ->> 'type' <> 'task' THEN
    RAISE EXCEPTION 'the reclassification reported %', res ->> 'type';
  END IF;
  -- A task is operational, so it queues for Beads rather than becoming a
  -- record. Which is the point of reclassifying: as a question it would have
  -- gone to the same place, but as a task it is somebody's work.
  IF res ->> 'published' <> 'queued_beads' THEN
    RAISE EXCEPTION 'the reclassified task published as %', res ->> 'published';
  END IF;

  SELECT * INTO row_out FROM knowledge_candidates WHERE id = cand;
  IF row_out.candidate_type <> 'task' THEN
    RAISE EXCEPTION 'the row still says %', row_out.candidate_type;
  END IF;
  IF row_out.proposed_owner_user_id IS NULL THEN
    RAISE EXCEPTION 'the owner was not assigned';
  END IF;
  IF row_out.proposed_project_id <> current_setting('test.alpha')::uuid THEN
    RAISE EXCEPTION 'the project was not assigned';
  END IF;

  -- Tightening a classification is allowed.
  SELECT id INTO cand FROM knowledge_candidates WHERE statement = 'The pilot has two cores';
  res := review_candidate(cand, 'accept', 'this is client material', NULL,
                          NULL, NULL, NULL, 'restricted');
  IF res ->> 'visibility' <> 'restricted' THEN
    RAISE EXCEPTION 'tightening reported %', res ->> 'visibility';
  END IF;
  -- And the record it published inherits the TIGHTENED classification, not the
  -- one the extractor guessed. Otherwise tightening at review would be
  -- cosmetic.
  SELECT count(*) INTO n FROM knowledge_records
   WHERE id = (res ->> 'record_id')::uuid AND visibility = 'restricted';
  IF n <> 1 THEN
    RAISE EXCEPTION 'the published record did not inherit the tightened classification';
  END IF;

  -- Widening is refused. knowledge.classification.downgrade is an ungrantable
  -- API scope precisely so this cannot happen through a token; the review
  -- surface must not be the back door.
  SELECT id INTO cand FROM knowledge_candidates WHERE statement = 'Widening is refused';
  BEGIN
    PERFORM review_candidate(cand, 'accept', NULL, NULL, NULL, NULL, NULL, 'internal');
    caught := '(nothing raised)';
  EXCEPTION WHEN others THEN
    caught := sqlerrm;
  END;
  IF caught NOT LIKE '%not widen it%' THEN
    RAISE EXCEPTION 'widening a classification at review reported: %', caught;
  END IF;
  -- And the refusal left no review behind: a decision that could not be
  -- applied must not be recorded as having happened.
  SELECT count(*) INTO n FROM knowledge_reviews WHERE candidate_id = cand;
  IF n <> 0 THEN
    RAISE EXCEPTION 'a refused review was recorded anyway';
  END IF;
  SELECT status INTO caught FROM knowledge_candidates WHERE id = cand;
  IF caught <> 'pending' THEN
    RAISE EXCEPTION 'a refused review left the candidate %', caught;
  END IF;

  -- Assigning to a project the reviewer cannot read is refused, and fails as
  -- "no such project" rather than confirming it exists.
  SELECT id INTO cand FROM knowledge_candidates
   WHERE statement = 'Assigning to an unreadable project is refused';
  BEGIN
    PERFORM review_candidate(cand, 'accept', NULL, NULL, NULL, NULL, 'test-corr-beta');
    caught := '(nothing raised)';
  EXCEPTION WHEN others THEN
    caught := sqlerrm;
  END;
  IF caught NOT LIKE '%no project with the slug%' AND caught NOT LIKE '%may not assign%' THEN
    RAISE EXCEPTION 'assigning to an unreadable project reported: %', caught;
  END IF;

  -- A mistyped owner is refused rather than silently left as the extractor's
  -- guess, which is how a mistyped owner becomes nobody's work.
  BEGIN
    PERFORM review_candidate(cand, 'accept', NULL, NULL, NULL, 'nobody@example.invalid');
    caught := '(nothing raised)';
  EXCEPTION WHEN others THEN
    caught := sqlerrm;
  END;
  IF caught NOT LIKE '%no user with the email%' THEN
    RAISE EXCEPTION 'an unknown owner reported: %', caught;
  END IF;

  -- Clearing is distinguishable from saying nothing.
  res := review_candidate(cand, 'accept', NULL, NULL, NULL, NULL, NULL, NULL, true, true);
  SELECT * INTO row_out FROM knowledge_candidates WHERE id = cand;
  IF row_out.proposed_owner_user_id IS NOT NULL OR row_out.proposed_project_id IS NOT NULL THEN
    RAISE EXCEPTION 'clearing the owner and project left them set';
  END IF;

  RAISE NOTICE 'reviewer correction assertions passed';
END $$;

-- A company-scoped record that is not internal must be readable by SOMEBODY.
--
-- The read policy from 0008 was
--
--   (scope = 'company' AND visibility = 'internal') OR can_read_project(project_id)
--
-- and can_read_project(NULL) is false, so a company-scoped confidential record
-- satisfied neither branch and was readable by nobody at all. It surfaced here
-- because review_candidate uses RETURNING, and RETURNING has to satisfy the
-- read policy -- so tightening a company-scoped statement failed outright
-- instead of quietly writing a record nothing could retrieve.
DO $$
DECLARE cand uuid; res jsonb; rec uuid; n integer;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.reviewer'), true);

  INSERT INTO knowledge_candidates (source_id, candidate_type, statement, confidence,
                                    visibility, source_spans, extractor_prompt_version)
  VALUES (current_setting('test.source')::uuid, 'lesson',
          'A company-scoped confidential record is readable by management', 0.9,
          'internal', '[{"line":7,"text":"said it"}]'::jsonb, 'extract-test')
  RETURNING id INTO cand;

  -- Tightened to confidential, and company-scoped because the candidate names
  -- no project.
  res := review_candidate(cand, 'accept', 'sensitive', NULL,
                          NULL, NULL, NULL, 'confidential');
  rec := (res ->> 'record_id')::uuid;
  IF rec IS NULL THEN
    RAISE EXCEPTION 'tightening a company-scoped record published nothing: %', res;
  END IF;

  -- The reviewer who accepted it holds organisation_admin, so they read it.
  SELECT count(*) INTO n FROM knowledge_records
   WHERE id = rec AND scope = 'company' AND visibility = 'confidential';
  IF n <> 1 THEN
    RAISE EXCEPTION 'the record it just published is invisible to its own reviewer';
  END IF;

  -- And somebody with no company-wide grant does not: confidential means
  -- narrower than everyone, and with no project there is no membership to
  -- scope it to, so the company-wide role holders are the readers.
  PERFORM set_config('workgraph.user_id', current_setting('test.outsider'), true);
  SELECT count(*) INTO n FROM knowledge_records WHERE id = rec;
  IF n <> 0 THEN
    RAISE EXCEPTION 'a company-scoped confidential record is readable by everybody';
  END IF;

  -- An internal one is, which is what internal means.
  PERFORM set_config('workgraph.user_id', current_setting('test.reviewer'), true);
  SELECT count(*) INTO n FROM knowledge_records
   WHERE scope = 'company' AND visibility = 'internal';
  PERFORM set_config('workgraph.user_id', current_setting('test.outsider'), true);
  IF n > 0 THEN
    SELECT count(*) INTO n FROM knowledge_records
     WHERE scope = 'company' AND visibility = 'internal';
    IF n = 0 THEN
      RAISE EXCEPTION 'company-wide internal records are hidden from an ordinary user';
    END IF;
  END IF;

  RAISE NOTICE 'a company-scoped record is readable by the right people';
END $$;

-- The four-argument call still works. Every existing caller uses it, and a
-- migration that adds parameters must not break them.
DO $$
DECLARE org uuid; src uuid; cand uuid; res jsonb;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.reviewer'), true);
  INSERT INTO knowledge_candidates (source_id, candidate_type, statement, confidence,
                                    visibility, source_spans, extractor_prompt_version)
  VALUES (current_setting('test.source')::uuid, 'lesson', 'The old signature still resolves',
          0.9, 'confidential', '[{"line":9,"text":"said it"}]'::jsonb, 'extract-test')
  RETURNING id INTO cand;

  res := review_candidate(cand, 'edit_and_accept', 'reworded', 'The old signature resolves');
  IF res ->> 'status' <> 'edited_accepted' THEN
    RAISE EXCEPTION 'the four-argument call reported %', res ->> 'status';
  END IF;
  IF (SELECT statement FROM knowledge_candidates WHERE id = cand) <> 'The old signature resolves' THEN
    RAISE EXCEPTION 'the edit was not applied';
  END IF;

  RAISE NOTICE 'the four-argument call still resolves';
END $$;

ROLLBACK;
