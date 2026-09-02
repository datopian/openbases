-- An accepted operational candidate becomes work, exactly once (WP-H4).
--
-- Publication happens outside the database: creating a bead means running `bd`
-- against a directory on a host. So what is testable here is the whole
-- interface between the two -- which candidates are offered, which graph each
-- is routed to, which are refused and why, and that recording the result is
-- idempotent. The publisher's own behaviour is tested in internal/publish.
--
-- Routing is a confidentiality boundary, not a convenience: a project's graph
-- is visible to that project, and the company graph is visible to everybody.

BEGIN;

DO $$
DECLARE org uuid; src uuid; cdt uuid; nged uuid;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';
  SELECT id INTO cdt FROM projects WHERE slug = 'cdt';
  SELECT id INTO nged FROM projects WHERE slug = 'nged';

  -- A graph for cdt with a real path, one for nged with none, and the company
  -- graph. Together they cover every routing outcome.
  INSERT INTO beads_databases (organisation_id, name, scope, project_id, path, host)
  VALUES (org, 'test-project-cdt', 'project', cdt, '/srv/graphs/test-project-cdt', 'test-control'),
         (org, 'test-project-nged', 'project', nged, '', 'test-control'),
         (org, 'test-company-hq', 'company', NULL, '/srv/graphs/test-company-hq', 'test-control');

  INSERT INTO knowledge_sources (organisation_id, provider, provider_source_id,
                                 provider_revision, source_type, project_id,
                                 visibility, captured_at)
  VALUES (org, 'google-meet', 'test/publish-work', 'sha256:work', 'transcript',
          cdt, 'restricted', now())
  RETURNING id INTO src;
  PERFORM set_config('test.source', src::text, false);
  PERFORM set_config('test.lead',
    (SELECT primary_owner_id::text FROM projects WHERE slug = 'cdt'), false);

  INSERT INTO knowledge_candidates (source_id, candidate_type, statement, confidence,
                                    visibility, proposed_project_id, due_date, source_spans,
                                    extractor_prompt_version)
  VALUES
    -- Routable: a project with a graph.
    (src, 'task', 'Send CDT the revised scope', 0.9, 'restricted', cdt,
     current_date + 7, '[{"line":1,"text":"said it"}]'::jsonb, 'extract-test'),
    -- Unroutable: the project's graph has no recorded path.
    (src, 'commitment', 'Give NGED a date for the pilot', 0.9, 'restricted', nged,
     NULL, '[{"line":2,"text":"said it"}]'::jsonb, 'extract-test'),
    -- Unroutable: restricted with no project has nowhere narrow enough.
    (src, 'risk', 'A named client may not renew', 0.8, 'restricted', NULL,
     NULL, '[{"line":3,"text":"said it"}]'::jsonb, 'extract-test'),
    -- Routable to the company graph: internal, no project.
    (src, 'question', 'Do we need a second reviewer per project?', 0.7, 'internal', NULL,
     NULL, '[{"line":4,"text":"said it"}]'::jsonb, 'extract-test'),
    -- Durable, so never a bead.
    (src, 'decision', 'Batch ingestion for phase one', 0.95, 'restricted', cdt,
     NULL, '[{"line":5,"text":"said it"}]'::jsonb, 'extract-test'),
    -- Left pending here, rejected at the end, to prove a rejected candidate
    -- cannot acquire work.
    (src, 'task', 'A statement that gets rejected', 0.5, 'internal', cdt,
     NULL, '[{"line":6,"text":"said it"}]'::jsonb, 'extract-test');
END $$;

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE
  n integer; res jsonb; cand uuid; row_out record; ref uuid; ref2 uuid;
  graph uuid; before_refs integer; accepted integer;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.lead'), true);

  -- Nothing is offered before anything is accepted: the queue is driven by
  -- review, not by extraction.
  SELECT count(*) INTO n FROM system_pending_work_publications(50)
   WHERE candidate_id IN (SELECT id FROM knowledge_candidates
                           WHERE source_id = current_setting('test.source')::uuid);
  IF n <> 0 THEN
    RAISE EXCEPTION 'a pending candidate was offered for publication before review';
  END IF;

  accepted := 0;
  FOR cand IN
    SELECT id FROM knowledge_candidates
     WHERE source_id = current_setting('test.source')::uuid
       AND statement <> 'A statement that gets rejected'
     ORDER BY created_at
  LOOP
    res := review_candidate(cand, 'accept');
    IF res ->> 'status' <> 'accepted' THEN
      RAISE EXCEPTION 'acceptance reported %', res ->> 'status';
    END IF;
    accepted := accepted + 1;
  END LOOP;
  -- Asserted, because everything below reads a row and NULL comparisons are
  -- not TRUE: a loop that iterated zero times -- the reviewer's identity not
  -- set, so RLS showing an empty table -- would pass every check that follows
  -- by finding nothing at all.
  IF accepted <> 5 THEN
    RAISE EXCEPTION 'accepted % candidates, expected 5', accepted;
  END IF;

  -- The durable one published in the transaction and is not work.
  SELECT count(*) INTO n FROM system_pending_work_publications(50) p
    JOIN knowledge_candidates c ON c.id = p.candidate_id
   WHERE c.candidate_type = 'decision';
  IF n <> 0 THEN
    RAISE EXCEPTION 'a decision was offered as work; it belongs in durable memory';
  END IF;

  -- The cdt task routes to the cdt graph, with its due date and the reviewer
  -- who accepted it.
  SELECT * INTO row_out FROM system_pending_work_publications(50)
   WHERE statement = 'Send CDT the revised scope';
  IF row_out.candidate_id IS NULL THEN
    RAISE EXCEPTION 'the accepted cdt task was not offered for publication at all';
  END IF;
  IF row_out.graph_name <> 'test-project-cdt' THEN
    RAISE EXCEPTION 'the cdt task routed to graph %', coalesce(row_out.graph_name, '(none)');
  END IF;
  IF row_out.blocked_reason IS NOT NULL THEN
    RAISE EXCEPTION 'a routable task was blocked: %', row_out.blocked_reason;
  END IF;
  IF row_out.due_date IS NULL THEN
    RAISE EXCEPTION 'the publisher was not told the due date';
  END IF;
  IF row_out.graph_host IS DISTINCT FROM 'test-control' THEN
    RAISE EXCEPTION 'the publisher was told the graph is on host %',
      coalesce(row_out.graph_host, '(null)');
  END IF;
  IF row_out.reviewer_email IS NULL THEN
    RAISE EXCEPTION 'the accepting reviewer was not passed through as the actor';
  END IF;
  graph := row_out.graph_id;

  -- An internal, projectless question routes to the company graph.
  SELECT * INTO row_out FROM system_pending_work_publications(50)
   WHERE candidate_type = 'question';
  IF row_out.candidate_id IS NULL THEN
    RAISE EXCEPTION 'the accepted question was not offered for publication at all';
  END IF;
  IF row_out.graph_name <> 'test-company-hq' OR row_out.blocked_reason IS NOT NULL THEN
    RAISE EXCEPTION 'the company question routed to % (%)',
      coalesce(row_out.graph_name, '(none)'), coalesce(row_out.blocked_reason, 'no reason');
  END IF;

  -- A project whose graph has no path is refused rather than routed anywhere
  -- else. The company graph is not a fallback: it would put a client's
  -- commitment in front of the whole company.
  SELECT * INTO row_out FROM system_pending_work_publications(50)
   WHERE candidate_type = 'commitment';
  IF row_out.candidate_id IS NULL THEN
    RAISE EXCEPTION 'the accepted commitment was not offered for publication at all';
  END IF;
  IF row_out.blocked_reason IS NULL THEN
    RAISE EXCEPTION 'a commitment whose graph has no path was routed to %', row_out.graph_name;
  END IF;
  IF row_out.graph_name = 'test-company-hq' THEN
    RAISE EXCEPTION 'a client commitment fell back to the company graph';
  END IF;

  -- Restricted with no project: refused, for the same reason stated out loud.
  SELECT * INTO row_out FROM system_pending_work_publications(50)
   WHERE candidate_type = 'risk';
  IF row_out.candidate_id IS NULL THEN
    RAISE EXCEPTION 'the accepted risk was not offered for publication at all';
  END IF;
  IF row_out.blocked_reason IS NULL OR row_out.graph_id IS NOT NULL THEN
    RAISE EXCEPTION 'a restricted projectless risk was routed to %',
      coalesce(row_out.graph_name, '(none)');
  END IF;

  -- Recording the publication links the candidate to a work_ref.
  SELECT id INTO cand FROM knowledge_candidates
   WHERE statement = 'Send CDT the revised scope';
  ref := system_record_work_publication(cand, graph, 'cdt-abc', 'Send CDT the revised scope', 'task');

  SELECT count(*) INTO n FROM knowledge_candidates
   WHERE id = cand AND work_ref_id = ref;
  IF n <> 1 THEN
    RAISE EXCEPTION 'the candidate was not linked to the work reference';
  END IF;

  SELECT count(*) INTO n FROM work_refs
   WHERE id = ref AND bead_id = 'cdt-abc' AND beads_database_id = graph
     AND visibility = 'restricted' AND project_id = (SELECT id FROM projects WHERE slug = 'cdt');
  IF n <> 1 THEN
    RAISE EXCEPTION 'the work reference does not carry the graph, classification and project';
  END IF;

  -- Published once: it is no longer offered.
  SELECT count(*) INTO n FROM system_pending_work_publications(50)
   WHERE candidate_id = cand;
  IF n <> 0 THEN
    RAISE EXCEPTION 'a published candidate is still queued for publication';
  END IF;

  -- Idempotent. The publisher retries after a crash between creating the bead
  -- and recording it, and one accepted statement must not become two beads.
  SELECT count(*) INTO before_refs FROM work_refs;
  ref2 := system_record_work_publication(cand, graph, 'cdt-abc', 'Send CDT the revised scope', 'task');
  IF ref2 <> ref THEN
    RAISE EXCEPTION 'a second recording produced a different work reference';
  END IF;
  SELECT count(*) INTO n FROM work_refs;
  IF n <> before_refs THEN
    RAISE EXCEPTION 'a second recording created another work reference';
  END IF;

  RAISE NOTICE 'work publication assertions passed';
END $$;

-- A candidate nobody accepted cannot acquire work, even if a bead was somehow
-- created for it. The publisher only ever reads accepted rows, so this is the
-- guard against a caller that does not -- and the reason it RAISES rather than
-- filing the row quietly is that a bead already exists by then.
DO $$
DECLARE cand uuid; graph uuid; caught text;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.lead'), true);

  SELECT id INTO cand FROM knowledge_candidates
   WHERE statement = 'A statement that gets rejected';
  PERFORM review_candidate(cand, 'reject', 'not real');

  SELECT graph_id INTO graph FROM system_pending_work_publications(50)
   WHERE graph_name = 'test-project-cdt' LIMIT 1;
  IF graph IS NULL THEN
    -- Every routable candidate has been published by now, so take the graph
    -- from the reference that publication wrote rather than reading
    -- beads_databases directly, which RLS may hide from this role.
    SELECT beads_database_id INTO graph FROM work_refs WHERE bead_id = 'cdt-abc';
  END IF;
  IF graph IS NULL THEN
    RAISE EXCEPTION 'the test could not find the graph it just published into';
  END IF;

  BEGIN
    PERFORM system_record_work_publication(cand, graph, 'cdt-zzz', 'x', 'task');
    caught := '(nothing raised)';
  EXCEPTION WHEN others THEN
    caught := sqlerrm;
  END;
  IF caught NOT LIKE '%not accepted%' THEN
    RAISE EXCEPTION 'recording work for a rejected candidate reported: %', caught;
  END IF;

  RAISE NOTICE 'a rejected candidate cannot acquire work';
END $$;

ROLLBACK;
