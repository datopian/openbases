-- An accepted durable record is offered for Git, once, with its provenance
-- (WP-H4, plan section 14.5).
--
-- The pull request itself is a GitHub call, so what is testable here is the
-- interface: which records are offered, what provenance travels with them,
-- which are refused and why, and that recording the result twice does not
-- propose the same record again.
--
-- The provenance assertions are the point. RECORD-SCHEMA.md rule 1 is that a
-- record carries either a source or a named author, and rule 6 is that no raw
-- text is committed -- so the query hands the renderer a provider, a revision
-- and line references, and never an excerpt.

BEGIN;

DO $$
DECLARE org uuid; src uuid; reviewer uuid; outsider uuid; alpha uuid; cand uuid;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';
  SELECT primary_owner_id INTO reviewer FROM projects WHERE slug = 'cdt';
  PERFORM set_config('test.reviewer', reviewer::text, false);

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Record publication outsider', 'test-rec-outsider@example.invalid')
  RETURNING id INTO outsider;

  INSERT INTO projects (organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id)
  VALUES (org, 'test-rec-alpha', 'Record publication test', 'confidential',
          reviewer, outsider)
  RETURNING id INTO alpha;
  INSERT INTO project_memberships (project_id, user_id, role_name)
  VALUES (alpha, reviewer, 'contributor');
  PERFORM set_config('test.alpha', alpha::text, false);

  INSERT INTO beads_databases (organisation_id, name, scope, project_id, path, host)
  VALUES (org, 'test-rec-graph', 'project', alpha, '/srv/graphs/test-rec', 'test-control');

  INSERT INTO knowledge_sources (organisation_id, provider, provider_source_id,
                                 provider_revision, source_type, project_id,
                                 visibility, captured_at)
  VALUES (org, 'google-meet', 'test/record-publication', 'sha256:rec', 'transcript',
          alpha, 'confidential', now())
  RETURNING id INTO src;

  INSERT INTO knowledge_candidates (source_id, candidate_type, statement, confidence,
                                    visibility, proposed_project_id, source_spans,
                                    extractor_prompt_version)
  VALUES (src, 'decision', 'Use batch ingestion for phase one', 0.94, 'confidential',
          alpha, '[{"line":12,"text":"we will batch"},{"line":13,"text":"agreed"}]'::jsonb,
          'extract-test')
  RETURNING id INTO cand;
  PERFORM set_config('test.candidate', cand::text, false);

  -- A second candidate, left for the system-path block below: by then the
  -- first record has been published, and system_record_markdown_publication is
  -- idempotent, so reusing it would prove nothing.
  INSERT INTO knowledge_candidates (source_id, candidate_type, statement, confidence,
                                    visibility, proposed_project_id, source_spans,
                                    extractor_prompt_version)
  VALUES (src, 'lesson', 'Check the deferred triggers when a system write fails', 0.9,
          'confidential', alpha, '[{"line":20,"text":"noted"}]'::jsonb, 'extract-test')
  RETURNING id INTO cand;
  PERFORM set_config('test.candidate2', cand::text, false);

  -- A third, for the related_work block: the first record is fully published
  -- by then and out of the queue, so it cannot answer whether a record names
  -- its bead.
  INSERT INTO knowledge_candidates (source_id, candidate_type, statement, confidence,
                                    visibility, proposed_project_id, source_spans,
                                    extractor_prompt_version)
  VALUES (src, 'decision', 'A second decision', 0.9, 'confidential', alpha,
          '[{"line":30,"text":"agreed"}]'::jsonb, 'extract-test')
  RETURNING id INTO cand;
  PERFORM set_config('test.candidate3', cand::text, false);
END $$;

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE res jsonb; rec uuid; row_out record; n integer; recorded boolean;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.reviewer'), true);

  res := review_candidate(current_setting('test.candidate')::uuid, 'accept');
  rec := (res ->> 'record_id')::uuid;
  IF rec IS NULL THEN
    RAISE EXCEPTION 'accepting a decision produced no record: %', res;
  END IF;

  SELECT * INTO row_out FROM system_pending_record_publications(20)
   WHERE record_id = rec;
  IF row_out.record_id IS NULL THEN
    RAISE EXCEPTION 'an accepted record was not offered for Git at all';
  END IF;
  IF row_out.blocked_reason IS NOT NULL THEN
    RAISE EXCEPTION 'an accepted record was blocked: %', row_out.blocked_reason;
  END IF;

  -- Everything the front matter needs, and the classification unwidened.
  IF row_out.scope <> 'project' OR row_out.project_slug <> 'test-rec-alpha' THEN
    RAISE EXCEPTION 'scope came back as % / %', row_out.scope, row_out.project_slug;
  END IF;
  IF row_out.visibility <> 'confidential' THEN
    RAISE EXCEPTION 'the record is offered as % rather than confidential', row_out.visibility;
  END IF;
  IF row_out.reviewer_email IS NULL THEN
    RAISE EXCEPTION 'the record has no reviewer to attribute it to';
  END IF;
  IF row_out.candidate_id <> current_setting('test.candidate')::uuid THEN
    RAISE EXCEPTION 'the record does not point back at its candidate';
  END IF;
  IF row_out.organisation_id IS NULL THEN
    RAISE EXCEPTION 'the record has no organisation, so a bead tuple cannot be built';
  END IF;
  -- No bead yet, so nothing to link. Asserted because an empty list and a
  -- missing key are different things to a reader of the front matter.
  IF row_out.related_work <> '[]'::jsonb THEN
    RAISE EXCEPTION 'related_work is % before any bead exists', row_out.related_work;
  END IF;

  -- Provenance by reference. A provider, a revision, and line numbers.
  IF jsonb_array_length(row_out.sources) <> 1 THEN
    RAISE EXCEPTION 'sources = %', row_out.sources;
  END IF;
  IF row_out.sources -> 0 ->> 'provider' <> 'google-meet'
     OR row_out.sources -> 0 ->> 'provider_revision' <> 'sha256:rec'
     OR row_out.sources -> 0 ->> 'source_id' <> 'test/record-publication' THEN
    RAISE EXCEPTION 'the source reference is wrong: %', row_out.sources -> 0;
  END IF;
  IF row_out.sources -> 0 -> 'excerpt_refs' <> '["line:12", "line:13"]'::jsonb THEN
    RAISE EXCEPTION 'the span references are %', row_out.sources -> 0 -> 'excerpt_refs';
  END IF;

  -- The raw text must not travel. It is in source_spans, one join away, and
  -- the whole reason this function selects references is that the renderer
  -- must never be able to write it.
  IF row_out.sources::text LIKE '%we will batch%' THEN
    RAISE EXCEPTION 'the excerpt text reached the renderer: %', row_out.sources;
  END IF;

  -- The graph a decision bead would go into, by the operational routing.
  SELECT count(*) INTO n FROM beads_databases
   WHERE id = row_out.graph_id AND project_id = current_setting('test.alpha')::uuid;
  IF n <> 1 THEN
    RAISE EXCEPTION 'the decision would go into graph %', coalesce(row_out.graph_name, '(none)');
  END IF;

  -- Recording the publication takes it out of the queue.
  recorded := system_record_markdown_publication(rec,
      'knowledge/projects/test-rec-alpha/decisions/2026-09-02-use-batch-ingestion-abcd1234.md',
      'https://github.com/datopian/company-workgraph/pull/1');
  IF NOT recorded THEN
    RAISE EXCEPTION 'the first recording reported nothing to do';
  END IF;

  -- Still offered, because the decision's bead is outstanding -- and it says
  -- which half is done. Keying the queue on git_path alone dropped a decision
  -- whose pull request opened and whose bead then failed.
  SELECT * INTO row_out FROM system_pending_record_publications(20) WHERE record_id = rec;
  IF row_out.record_id IS NULL THEN
    RAISE EXCEPTION 'a decision with no bead left the queue once its Markdown was recorded';
  END IF;
  IF NOT row_out.markdown_published OR row_out.bead_published THEN
    RAISE EXCEPTION 'the halves are reported as markdown=% bead=%',
      row_out.markdown_published, row_out.bead_published;
  END IF;

  -- Recording the bead finishes it, and the record then names the bead it
  -- produced. The bead's description already carried "Record: mem-<id>"; the
  -- link ran one way, and the two artefacts exist so Git and PostgreSQL agree.
  PERFORM system_record_work_publication(current_setting('test.candidate')::uuid,
      row_out.graph_id, 'rec-abc', 'Use batch ingestion for phase one', 'decision');

  SELECT count(*) INTO n FROM system_pending_record_publications(20) WHERE record_id = rec;
  IF n <> 0 THEN
    RAISE EXCEPTION 'a record with both halves published is still queued';
  END IF;

  -- Idempotent: a pass that opened the pull request and then failed must not
  -- open a second one.
  recorded := system_record_markdown_publication(rec, 'knowledge/other.md',
      'https://github.com/datopian/company-workgraph/pull/2');
  IF recorded THEN
    RAISE EXCEPTION 'a second recording claimed to publish again';
  END IF;
  SELECT count(*) INTO n FROM knowledge_records
   WHERE id = rec AND git_pr_url = 'https://github.com/datopian/company-workgraph/pull/1';
  IF n <> 1 THEN
    RAISE EXCEPTION 'the second recording overwrote the first pull request';
  END IF;

  RAISE NOTICE 'record publication assertions passed';
END $$;

-- Once the bead exists the record names it, as the identity TUPLE the schema
-- asks for: a bead id alone is ambiguous between graphs (plan section 7.3).
-- Read through a second record so the first, now fully published, stays out of
-- the queue.
DO $$
DECLARE rec uuid; row_out record; graph uuid;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.reviewer'), true);

  SELECT beads_database_id INTO graph FROM work_refs WHERE bead_id = 'rec-abc';
  IF graph IS NULL THEN
    RAISE EXCEPTION 'the earlier block did not record a bead';
  END IF;

  rec := (review_candidate(current_setting('test.candidate3')::uuid, 'accept') ->> 'record_id')::uuid;
  PERFORM system_record_work_publication(current_setting('test.candidate3')::uuid,
      graph, 'rec-def', 'A second decision', 'decision');

  SELECT * INTO row_out FROM system_pending_record_publications(20) WHERE record_id = rec;
  IF row_out.record_id IS NULL THEN
    RAISE EXCEPTION 'a record with a bead and no Markdown left the queue';
  END IF;
  IF jsonb_array_length(row_out.related_work) <> 1 THEN
    RAISE EXCEPTION 'related_work = %', row_out.related_work;
  END IF;
  IF row_out.related_work -> 0 ->> 'bead_id' <> 'rec-def' THEN
    RAISE EXCEPTION 'the wrong bead: %', row_out.related_work -> 0;
  END IF;
  IF row_out.related_work -> 0 ->> 'beads_database_id' <> graph::text THEN
    RAISE EXCEPTION 'the tuple has no graph, so the bead id is ambiguous: %',
      row_out.related_work -> 0;
  END IF;
  IF row_out.related_work -> 0 ->> 'organisation_id' IS NULL THEN
    RAISE EXCEPTION 'the tuple has no organisation: %', row_out.related_work -> 0;
  END IF;

  RAISE NOTICE 'a record names the bead it produced';
END $$;

-- The publisher records a publication with NO app user, because it runs on a
-- timer as the system. That has to work, and it did not: the provenance
-- trigger is DEFERRABLE INITIALLY DEFERRED, so it fires at COMMIT -- back as
-- the session role, outside the SECURITY DEFINER function -- and its EXISTS
-- against knowledge_record_sources was filtered by row-level security. Whether
-- a correct record passed the check depended on who was writing. Two pull
-- requests were open in company-workgraph for records it said had no source.
DO $$
DECLARE rec uuid; recorded boolean;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.reviewer'), true);
  rec := (review_candidate(current_setting('test.candidate2')::uuid, 'accept') ->> 'record_id')::uuid;
  IF rec IS NULL THEN
    RAISE EXCEPTION 'accepting the second candidate produced no record';
  END IF;
  PERFORM set_config('test.record', rec::text, false);

  -- As the system: an empty identity, which is what a timer has.
  PERFORM set_config('workgraph.user_id', '', true);
  IF current_app_user() IS NOT NULL THEN
    RAISE EXCEPTION 'this block must run with no app user';
  END IF;

  recorded := system_record_markdown_publication(current_setting('test.record')::uuid,
      'knowledge/projects/test-rec-alpha/decisions/system-path.md',
      'https://github.test/pr/system');
  IF NOT recorded THEN
    RAISE EXCEPTION 'the system path recorded nothing';
  END IF;
END $$;

-- Forced here rather than at COMMIT, so the deferred provenance check runs
-- inside the test transaction. Without the fix this is where it raises.
SET CONSTRAINTS ALL IMMEDIATE;

DO $$
BEGIN
  RAISE NOTICE 'the system path can record a publication with no identity';
END $$;

-- A path with no pull request, or a pull request with no path, is refused by
-- the schema: the first is a file nobody proposed, the second is unfindable.
DO $$
DECLARE org uuid; me uuid; caught text; rec uuid;
BEGIN
  RESET ROLE;
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';
  SELECT primary_owner_id INTO me FROM projects WHERE slug = 'cdt';

  BEGIN
    INSERT INTO knowledge_records (organisation_id, record_type, statement, scope,
        reviewer_user_id, visibility, human_authored, author_user_id, git_path)
    VALUES (org, 'decision', 'A path with no pull request', 'company',
            me, 'internal', true, me, 'knowledge/company/decisions/x.md')
    RETURNING id INTO rec;
    caught := '(nothing raised)';
  EXCEPTION WHEN check_violation THEN
    caught := sqlerrm;
  END;
  IF caught NOT LIKE '%git_path_names_its_pull_request%' THEN
    RAISE EXCEPTION 'a half-recorded publication was accepted: %', caught;
  END IF;

  RAISE NOTICE 'a path and a pull request travel together';
END $$;

ROLLBACK;
