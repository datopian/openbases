-- Correcting company memory supersedes, it does not overwrite (WP-H4, plan 14.7).
--
-- RECORD-SCHEMA.md rule 3: "Supersede, never overwrite. Correcting a record
-- means publishing a new record with supersedes: set and marking the old one
-- status: superseded with superseded_by:. History is evidence."
--
-- The columns and constraints existed from 0002 and nothing could reach them,
-- which in practice means the first correction anybody makes is an UPDATE and
-- the evidence is gone.

BEGIN;

DO $$
DECLARE org uuid; src uuid; reviewer uuid; outsider uuid; alpha uuid; cand uuid;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';
  SELECT primary_owner_id INTO reviewer FROM projects WHERE slug = 'cdt';
  PERFORM set_config('test.reviewer', reviewer::text, false);

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Supersession outsider', 'test-sup-outsider@example.invalid')
  RETURNING id INTO outsider;
  PERFORM set_config('test.outsider', outsider::text, false);

  INSERT INTO projects (organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id)
  VALUES (org, 'test-sup-alpha', 'Supersession test', 'confidential',
          reviewer, outsider)
  RETURNING id INTO alpha;
  INSERT INTO project_memberships (project_id, user_id, role_name)
  VALUES (alpha, reviewer, 'contributor');
  PERFORM set_config('test.alpha', alpha::text, false);

  INSERT INTO knowledge_sources (organisation_id, provider, provider_source_id,
                                 provider_revision, source_type, project_id,
                                 visibility, captured_at)
  VALUES (org, 'google-meet', 'test/supersession', 'sha256:sup', 'transcript',
          alpha, 'confidential', now())
  RETURNING id INTO src;

  INSERT INTO knowledge_candidates (source_id, candidate_type, statement, confidence,
                                    visibility, proposed_project_id, source_spans,
                                    extractor_prompt_version)
  VALUES (src, 'constraint', 'The pilot runs on two cores', 0.9, 'confidential',
          alpha, '[{"line":4,"text":"two cores"}]'::jsonb, 'extract-test')
  RETURNING id INTO cand;
  PERFORM set_config('test.candidate', cand::text, false);
END $$;

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE res jsonb; old uuid; new_id uuid; row_out record; n integer; caught text;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.reviewer'), true);

  res := review_candidate(current_setting('test.candidate')::uuid, 'accept');
  old := (res ->> 'record_id')::uuid;
  IF old IS NULL THEN
    RAISE EXCEPTION 'accepting a constraint produced no record: %', res;
  END IF;

  -- A reason is required. "The old one was wrong" without saying how is a
  -- record of an edit, not of a correction, and the reason IS the audit
  -- history this asks for.
  BEGIN
    PERFORM supersede_record(old, 'The pilot runs on four cores', '');
    caught := '(nothing raised)';
  EXCEPTION WHEN others THEN
    caught := sqlerrm;
  END;
  IF caught NOT LIKE '%needs a reason%' THEN
    RAISE EXCEPTION 'superseding without a reason reported: %', caught;
  END IF;

  res := supersede_record(old, 'The pilot runs on four cores',
                          'The node was resized on 2026-09-01');
  new_id := (res ->> 'record_id')::uuid;
  IF new_id IS NULL OR new_id = old THEN
    RAISE EXCEPTION 'supersession returned %', res;
  END IF;

  -- The old record survives, retired and naming its successor.
  SELECT * INTO row_out FROM knowledge_records WHERE id = old;
  IF row_out.status <> 'superseded' THEN
    RAISE EXCEPTION 'the old record is %', row_out.status;
  END IF;
  IF row_out.superseded_by_id <> new_id THEN
    RAISE EXCEPTION 'the old record does not name its successor';
  END IF;
  IF row_out.statement <> 'The pilot runs on two cores' THEN
    RAISE EXCEPTION 'the old statement was overwritten: %', row_out.statement;
  END IF;

  -- The new record inherits type, scope and classification, and names what it
  -- replaced. Inheriting the classification is what stops a correction being
  -- the way restricted material becomes internal.
  SELECT * INTO row_out FROM knowledge_records WHERE id = new_id;
  IF row_out.supersedes_id <> old THEN
    RAISE EXCEPTION 'the new record does not name what it supersedes';
  END IF;
  IF row_out.record_type <> 'constraint' OR row_out.scope <> 'project'
     OR row_out.visibility <> 'confidential' THEN
    RAISE EXCEPTION 'the correction changed type, scope or classification: % % %',
      row_out.record_type, row_out.scope, row_out.visibility;
  END IF;
  IF row_out.project_id <> current_setting('test.alpha')::uuid THEN
    RAISE EXCEPTION 'the correction moved project';
  END IF;
  IF row_out.reviewer_user_id <> current_setting('test.reviewer')::uuid THEN
    RAISE EXCEPTION 'the correction is attributed to somebody else';
  END IF;
  IF NOT row_out.human_authored OR row_out.author_user_id IS NULL THEN
    RAISE EXCEPTION 'a correction is somebody''s own words and must name them';
  END IF;
  -- A constraint expires; the successor gets its own horizon rather than
  -- inheriting a date already in the past.
  IF row_out.review_after IS DISTINCT FROM current_date + 365 THEN
    RAISE EXCEPTION 'the successor expires on %', row_out.review_after;
  END IF;

  -- Provenance travels: the sources that led to the original claim are still
  -- why the subject came up.
  SELECT count(*) INTO n FROM knowledge_record_sources WHERE record_id = new_id;
  IF n <> 1 THEN
    RAISE EXCEPTION 'the successor carries % sources', n;
  END IF;

  -- Audited, with the reason.
  SELECT count(*) INTO n FROM audit_log
   WHERE action = 'knowledge.record.supersede' AND target_id = old::text
     AND outcome = 'executed' AND reason = 'The node was resized on 2026-09-01'
     AND actor_user_id = current_setting('test.reviewer')::uuid;
  IF n <> 1 THEN
    RAISE EXCEPTION 'the supersession was not audited';
  END IF;

  -- Twice is refused: there is nothing live left to supersede, and a chain
  -- with two successors for one record is not history anybody can read.
  BEGIN
    PERFORM supersede_record(old, 'The pilot runs on eight cores', 'again');
    caught := '(nothing raised)';
  EXCEPTION WHEN others THEN
    caught := sqlerrm;
  END;
  IF caught NOT LIKE '%nothing live to supersede%' THEN
    RAISE EXCEPTION 'superseding a retired record reported: %', caught;
  END IF;

  -- The successor is offered for Git and carries the retired record's path
  -- when it has one, so the correction and the retirement are one pull request.
  UPDATE knowledge_records
     SET git_path = 'knowledge/projects/test-sup-alpha/constraints/old.md',
         git_pr_url = 'https://github.test/pr/1'
   WHERE id = old;

  SELECT * INTO row_out FROM system_pending_record_publications(20)
   WHERE record_id = new_id;
  IF row_out.record_id IS NULL THEN
    RAISE EXCEPTION 'the correction was not offered for Git';
  END IF;
  IF row_out.supersedes <> old THEN
    RAISE EXCEPTION 'the offered record does not name what it supersedes';
  END IF;
  IF row_out.supersedes_git_path <> 'knowledge/projects/test-sup-alpha/constraints/old.md' THEN
    RAISE EXCEPTION 'the retired file is %', coalesce(row_out.supersedes_git_path, '(none)');
  END IF;

  RAISE NOTICE 'supersession assertions passed';
END $$;

-- Somebody who cannot read a record cannot correct it. supersede_record runs
-- with invoker rights precisely so this is decided by the same policy as
-- reading, rather than by a check somebody remembered to write.
DO $$
DECLARE caught text; old uuid;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.reviewer'), true);
  SELECT id INTO old FROM knowledge_records
   WHERE project_id = current_setting('test.alpha')::uuid AND status = 'accepted'
   LIMIT 1;
  PERFORM set_config('test.record', old::text, false);

  PERFORM set_config('workgraph.user_id', current_setting('test.outsider'), true);
  BEGIN
    PERFORM supersede_record(current_setting('test.record')::uuid, 'Something else', 'because');
    caught := '(nothing raised)';
  EXCEPTION WHEN others THEN
    caught := sqlerrm;
  END;
  IF caught NOT LIKE '%no such record%' THEN
    RAISE EXCEPTION 'an outsider superseding a record they cannot read reported: %', caught;
  END IF;

  RAISE NOTICE 'only somebody who can read a record may correct it';
END $$;

ROLLBACK;
