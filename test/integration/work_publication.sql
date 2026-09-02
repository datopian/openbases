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
--
-- Every project and graph here is created by the test and rolled back. Naming
-- a real project would make the assertions depend on which graphs happen to be
-- registered in the environment -- which is how a first version passed in CI
-- and failed on staging, where the real project-cdt graph outranked the test's.

BEGIN;

DO $$
DECLARE
  org uuid; src uuid; reviewer uuid; outsider uuid;
  alpha uuid; beta uuid; gamma uuid;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';
  -- The reviewer is a real person, and a member of the test projects below.
  SELECT primary_owner_id INTO reviewer FROM projects WHERE slug = 'cdt';
  PERFORM set_config('test.reviewer', reviewer::text, false);

  -- The outsider is created here rather than borrowed. Every real person who
  -- looked like an outsider turned out to hold organisation_admin or
  -- executive, which can_read_project honours company-wide -- so the
  -- assertion passed for the wrong reason on a fresh database and failed on
  -- staging. A user with no memberships and no role grants is an outsider by
  -- construction.
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Publication test outsider', 'test-pub-outsider@example.invalid')
  RETURNING id INTO outsider;
  PERFORM set_config('test.outsider', outsider::text, false);

  -- Confidential rather than restricted: a restricted project must have its
  -- own execution cell (plan section 7.4), and this test is not about cells.
  INSERT INTO projects (organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id)
  VALUES (org, 'test-pub-alpha', 'Publication test: routable', 'confidential',
          reviewer, outsider)
  RETURNING id INTO alpha;
  INSERT INTO projects (organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id)
  VALUES (org, 'test-pub-beta', 'Publication test: graph with no path', 'confidential',
          reviewer, outsider)
  RETURNING id INTO beta;
  INSERT INTO projects (organisation_id, slug, name, visibility,
                        primary_owner_id, backup_owner_id)
  VALUES (org, 'test-pub-gamma', 'Publication test: no graph at all', 'confidential',
          reviewer, outsider)
  RETURNING id INTO gamma;

  -- Ownership is not readership: can_read_project asks about membership.
  INSERT INTO project_memberships (project_id, user_id, role_name)
  VALUES (alpha, reviewer, 'contributor'),
         (beta, reviewer, 'contributor'),
         (gamma, reviewer, 'contributor');

  -- One graph provisioned, one registered but provisioned nowhere, and none at
  -- all for gamma. Between them they cover every routing outcome.
  INSERT INTO beads_databases (organisation_id, name, scope, project_id, path, host)
  VALUES (org, 'test-alpha-graph', 'project', alpha, '/srv/graphs/test-alpha', 'test-control'),
         (org, 'test-beta-graph', 'project', beta, '', 'test-control');

  PERFORM set_config('test.alpha', alpha::text, false);

  INSERT INTO knowledge_sources (organisation_id, provider, provider_source_id,
                                 provider_revision, source_type, project_id,
                                 visibility, captured_at)
  VALUES (org, 'google-meet', 'test/publish-work', 'sha256:work', 'transcript',
          alpha, 'confidential', now())
  RETURNING id INTO src;
  PERFORM set_config('test.source', src::text, false);

  -- Candidates inherit readability from the SOURCE, so all of these are
  -- visible to the reviewer even when the candidate itself names no project.
  INSERT INTO knowledge_candidates (source_id, candidate_type, statement, confidence,
                                    visibility, proposed_project_id, due_date, source_spans,
                                    extractor_prompt_version)
  VALUES
    -- Routable: a project whose graph is provisioned.
    (src, 'task', 'Send alpha the revised scope', 0.9, 'confidential', alpha,
     current_date + 7, '[{"line":1,"text":"said it"}]'::jsonb, 'extract-test'),
    -- Registered, provisioned nowhere.
    (src, 'commitment', 'Give beta a date for the pilot', 0.9, 'confidential', beta,
     NULL, '[{"line":2,"text":"said it"}]'::jsonb, 'extract-test'),
    -- No graph for the project at all.
    (src, 'market-signal', 'Gamma is hiring in our space', 0.6, 'confidential', gamma,
     NULL, '[{"line":3,"text":"said it"}]'::jsonb, 'extract-test'),
    -- Restricted with no project: nowhere narrow enough exists.
    (src, 'risk', 'A named client may not renew', 0.8, 'restricted', NULL,
     NULL, '[{"line":4,"text":"said it"}]'::jsonb, 'extract-test'),
    -- Internal with no project: the company graph.
    (src, 'question', 'Do we need a second reviewer per project?', 0.7, 'internal', NULL,
     NULL, '[{"line":5,"text":"said it"}]'::jsonb, 'extract-test'),
    -- Durable, so never a bead.
    (src, 'decision', 'Batch ingestion for phase one', 0.95, 'confidential', alpha,
     NULL, '[{"line":6,"text":"said it"}]'::jsonb, 'extract-test'),
    -- Left pending here, rejected at the end.
    (src, 'task', 'A statement that gets rejected', 0.5, 'internal', alpha,
     NULL, '[{"line":7,"text":"said it"}]'::jsonb, 'extract-test');
END $$;

-- A company graph has to exist for the projectless internal case to route
-- anywhere. In a deployed environment one already does, and the test must not
-- assume its name -- so it asserts the SHAPE of the graph chosen (company
-- scope, no project) rather than which one won.
DO $$
DECLARE org uuid; n integer;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';
  SELECT count(*) INTO n FROM beads_databases
   WHERE scope = 'company' AND project_id IS NULL AND coalesce(path, '') <> '';
  IF n = 0 THEN
    INSERT INTO beads_databases (organisation_id, name, scope, project_id, path, host)
    VALUES (org, 'test-company-graph', 'company', NULL, '/srv/graphs/test-company', 'test-control');
  END IF;
END $$;

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE
  n integer; res jsonb; cand uuid; row_out record; ref uuid; ref2 uuid;
  graph uuid; before_refs integer; accepted integer;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.reviewer'), true);

  -- Nothing is offered before anything is accepted: the queue is driven by
  -- review, not by extraction.
  SELECT count(*) INTO n FROM system_pending_work_publications(50)
   WHERE source_id = current_setting('test.source')::uuid;
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
  IF accepted <> 6 THEN
    RAISE EXCEPTION 'accepted % candidates, expected 6', accepted;
  END IF;

  -- The durable one published in the transaction and is not work.
  SELECT count(*) INTO n FROM system_pending_work_publications(50)
   WHERE candidate_type = 'decision'
     AND source_id = current_setting('test.source')::uuid;
  IF n <> 0 THEN
    RAISE EXCEPTION 'a decision was offered as work; it belongs in durable memory';
  END IF;

  -- alpha's task routes to alpha's graph, with its due date, its host and the
  -- reviewer who accepted it.
  SELECT * INTO row_out FROM system_pending_work_publications(50)
   WHERE statement = 'Send alpha the revised scope';
  IF row_out.candidate_id IS NULL THEN
    RAISE EXCEPTION 'the accepted task was not offered for publication at all';
  END IF;
  IF row_out.blocked_reason IS NOT NULL THEN
    RAISE EXCEPTION 'a routable task was blocked: %', row_out.blocked_reason;
  END IF;
  SELECT count(*) INTO n FROM beads_databases
   WHERE id = row_out.graph_id AND project_id = current_setting('test.alpha')::uuid;
  IF n <> 1 THEN
    RAISE EXCEPTION 'the task routed to a graph that is not the project''s: %',
      coalesce(row_out.graph_name, '(none)');
  END IF;
  IF row_out.due_date IS NULL THEN
    RAISE EXCEPTION 'the publisher was not told the due date';
  END IF;
  IF coalesce(row_out.graph_path, '') = '' OR coalesce(row_out.graph_host, '') = '' THEN
    RAISE EXCEPTION 'the publisher was not told where the graph is';
  END IF;
  IF row_out.reviewer_email IS NULL THEN
    RAISE EXCEPTION 'the accepting reviewer was not passed through as the actor';
  END IF;
  graph := row_out.graph_id;

  -- An internal, projectless question routes to a company graph -- whichever
  -- one is registered, not a named one.
  SELECT * INTO row_out FROM system_pending_work_publications(50)
   WHERE candidate_type = 'question';
  IF row_out.candidate_id IS NULL THEN
    RAISE EXCEPTION 'the accepted question was not offered for publication at all';
  END IF;
  IF row_out.blocked_reason IS NOT NULL THEN
    RAISE EXCEPTION 'the company question was blocked: %', row_out.blocked_reason;
  END IF;
  SELECT count(*) INTO n FROM beads_databases
   WHERE id = row_out.graph_id AND scope = 'company' AND project_id IS NULL;
  IF n <> 1 THEN
    RAISE EXCEPTION 'the company question routed to a project graph: %',
      coalesce(row_out.graph_name, '(none)');
  END IF;

  -- A project whose graph has no path is refused rather than routed anywhere
  -- else, and says which of the two problems it is: registering a graph and
  -- provisioning one are different fixes.
  SELECT * INTO row_out FROM system_pending_work_publications(50)
   WHERE candidate_type = 'commitment';
  IF row_out.candidate_id IS NULL THEN
    RAISE EXCEPTION 'the accepted commitment was not offered for publication at all';
  END IF;
  IF row_out.blocked_reason NOT LIKE '%no path%' THEN
    RAISE EXCEPTION 'a commitment whose graph has no path reported: %',
      coalesce(row_out.blocked_reason, '(routed to ' || coalesce(row_out.graph_name, '?') || ')');
  END IF;
  IF row_out.graph_id IS NOT NULL THEN
    RAISE EXCEPTION 'a blocked commitment was handed a graph anyway';
  END IF;

  -- A project with no graph at all. The company graph is not a fallback: it
  -- would put a client's work in front of the whole company.
  SELECT * INTO row_out FROM system_pending_work_publications(50)
   WHERE candidate_type = 'market-signal';
  IF row_out.blocked_reason NOT LIKE '%no graph is registered for project test-pub-gamma%' THEN
    RAISE EXCEPTION 'a candidate for a project with no graph reported: %',
      coalesce(row_out.blocked_reason, '(routed to ' || coalesce(row_out.graph_name, '?') || ')');
  END IF;

  -- Restricted with no project: refused, for the same reason stated out loud.
  SELECT * INTO row_out FROM system_pending_work_publications(50)
   WHERE candidate_type = 'risk';
  IF row_out.blocked_reason NOT LIKE '%restricted with no project%' THEN
    RAISE EXCEPTION 'a restricted projectless risk reported: %',
      coalesce(row_out.blocked_reason, '(routed to ' || coalesce(row_out.graph_name, '?') || ')');
  END IF;
  IF row_out.graph_id IS NOT NULL THEN
    RAISE EXCEPTION 'a restricted projectless risk was handed a graph anyway';
  END IF;

  -- Recording the publication links the candidate to a work_ref.
  SELECT id INTO cand FROM knowledge_candidates
   WHERE statement = 'Send alpha the revised scope';
  ref := system_record_work_publication(cand, graph, 'alp-abc', 'Send alpha the revised scope', 'task');

  SELECT count(*) INTO n FROM knowledge_candidates
   WHERE id = cand AND work_ref_id = ref;
  IF n <> 1 THEN
    RAISE EXCEPTION 'the candidate was not linked to the work reference';
  END IF;

  SELECT count(*) INTO n FROM work_refs
   WHERE id = ref AND bead_id = 'alp-abc' AND beads_database_id = graph
     AND visibility = 'confidential'
     AND project_id = current_setting('test.alpha')::uuid;
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
  ref2 := system_record_work_publication(cand, graph, 'alp-abc', 'Send alpha the revised scope', 'task');
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
  PERFORM set_config('workgraph.user_id', current_setting('test.reviewer'), true);

  SELECT id INTO cand FROM knowledge_candidates
   WHERE statement = 'A statement that gets rejected';
  PERFORM review_candidate(cand, 'reject', 'not real');

  SELECT beads_database_id INTO graph FROM work_refs WHERE bead_id = 'alp-abc';
  IF graph IS NULL THEN
    RAISE EXCEPTION 'the test could not find the graph it just published into';
  END IF;

  BEGIN
    PERFORM system_record_work_publication(cand, graph, 'alp-zzz', 'x', 'task');
    caught := '(nothing raised)';
  EXCEPTION WHEN others THEN
    caught := sqlerrm;
  END;
  IF caught NOT LIKE '%not accepted%' THEN
    RAISE EXCEPTION 'recording work for a rejected candidate reported: %', caught;
  END IF;

  RAISE NOTICE 'a rejected candidate cannot acquire work';
END $$;

-- The graph registry is client information, and 0057 made it more so: it used
-- to hold a row per execution cell, and now holds a graph name, an absolute
-- path and a hostname per PROJECT. 0023 already put beads_databases behind the
-- project predicate; this asserts it, because the table's contents changed
-- under a policy nobody re-read.
DO $$
DECLARE n integer;
BEGIN
  PERFORM set_config('workgraph.user_id', current_setting('test.outsider'), true);

  SELECT count(*) INTO n FROM beads_databases WHERE name = 'test-alpha-graph';
  IF n <> 0 THEN
    RAISE EXCEPTION 'somebody outside the project can see its graph, path and host';
  END IF;

  -- Not a blanket denial: a company graph is company-wide by definition.
  SELECT count(*) INTO n FROM beads_databases
   WHERE scope = 'company' AND project_id IS NULL;
  IF n = 0 THEN
    RAISE EXCEPTION 'the company graph is hidden from somebody with no project';
  END IF;

  RAISE NOTICE 'the graph registry follows the project predicate';
END $$;

ROLLBACK;
