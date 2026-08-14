-- Cross-project leakage test. WP-C1 acceptance: "authorisation tests cover
-- cross-project leakage attempts".
--
-- Seeds two projects with disjoint membership, then asserts as the application
-- role that each user sees only their own. Every assertion raises rather than
-- returning a row, so a silent pass is impossible.

-- Uses a suite-specific organisation slug so the suite runs against a database
-- that already carries the pilot seed, not only against an empty one. A test
-- that only passes on an empty database cannot check production-shaped state.
BEGIN;

-- ---------------------------------------------------------------------------
-- Seed
-- ---------------------------------------------------------------------------

INSERT INTO organisations (id, slug, name)
VALUES ('00000000-0000-0000-0000-0000000000a1', 'test-rls', 'Test org: rls_isolation');

INSERT INTO users (id, organisation_id, display_name, primary_email) VALUES
  ('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000a1', 'Alice', 'alice@example.com'),
  ('00000000-0000-0000-0000-0000000000b2', '00000000-0000-0000-0000-0000000000a1', 'Bob',   'bob@example.com'),
  ('00000000-0000-0000-0000-0000000000b3', '00000000-0000-0000-0000-0000000000a1', 'Backup','backup@example.com'),
  ('00000000-0000-0000-0000-0000000000b4', '00000000-0000-0000-0000-0000000000a1', 'Mallory','mallory@example.com');

INSERT INTO execution_nodes (id, hostname, environment)
VALUES ('00000000-0000-0000-0000-0000000000c0', 'exec-1.staging', 'staging');

INSERT INTO execution_cells (id, execution_node_id, slug, system_username, trust_domain) VALUES
  ('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-0000000000c0', 'cell-oss',    'wgcell_oss',    'oss'),
  ('00000000-0000-0000-0000-0000000000c2', '00000000-0000-0000-0000-0000000000c0', 'cell-client', 'wgcell_client', 'client-acme');

INSERT INTO projects (id, organisation_id, slug, name, visibility, primary_owner_id, backup_owner_id, execution_cell_id) VALUES
  ('00000000-0000-0000-0000-0000000000d1', '00000000-0000-0000-0000-0000000000a1', 'oss-portal', 'OSS Portal',
   'internal',   '00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000b3', '00000000-0000-0000-0000-0000000000c1'),
  ('00000000-0000-0000-0000-0000000000d2', '00000000-0000-0000-0000-0000000000a1', 'acme-delivery', 'Acme Delivery',
   'restricted', '00000000-0000-0000-0000-0000000000b2', '00000000-0000-0000-0000-0000000000b3', '00000000-0000-0000-0000-0000000000c2');

INSERT INTO project_memberships (project_id, user_id, role_name) VALUES
  ('00000000-0000-0000-0000-0000000000d1', '00000000-0000-0000-0000-0000000000b1', 'project_lead'),
  ('00000000-0000-0000-0000-0000000000d2', '00000000-0000-0000-0000-0000000000b2', 'project_lead');

INSERT INTO beads_databases (id, organisation_id, name, scope, project_id) VALUES
  ('00000000-0000-0000-0000-0000000000e1', '00000000-0000-0000-0000-0000000000a1', 'oss', 'project', '00000000-0000-0000-0000-0000000000d1'),
  ('00000000-0000-0000-0000-0000000000e2', '00000000-0000-0000-0000-0000000000a1', 'acme','project', '00000000-0000-0000-0000-0000000000d2');

INSERT INTO work_refs (organisation_id, execution_cell_id, beads_database_id, bead_id, title, project_id, visibility) VALUES
  ('00000000-0000-0000-0000-0000000000a1', '00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-0000000000e1', 'oss-1',  'Public work',   '00000000-0000-0000-0000-0000000000d1', 'internal'),
  ('00000000-0000-0000-0000-0000000000a1', '00000000-0000-0000-0000-0000000000c2', '00000000-0000-0000-0000-0000000000e2', 'acme-1', 'Client secret', '00000000-0000-0000-0000-0000000000d2', 'restricted');

INSERT INTO knowledge_sources (id, organisation_id, provider, provider_source_id, source_type, project_id, visibility) VALUES
  ('00000000-0000-0000-0000-0000000000f2', '00000000-0000-0000-0000-0000000000a1', 'google-meet', 'spaces/acme-standup',
   'transcript', '00000000-0000-0000-0000-0000000000d2', 'restricted');

INSERT INTO knowledge_candidates (source_id, candidate_type, statement, confidence, visibility, source_spans)
VALUES ('00000000-0000-0000-0000-0000000000f2', 'decision', 'Acme will migrate in Q4.', 0.910, 'restricted', '[{"start":10,"end":48}]'::jsonb);

INSERT INTO knowledge_records (id, organisation_id, record_type, statement, scope, project_id,
                               owner_user_id, reviewer_user_id, visibility, review_after, human_authored, author_user_id)
VALUES ('00000000-0000-0000-0000-000000000f10', '00000000-0000-0000-0000-0000000000a1', 'fact',
        'Acme runs on-premise only.', 'project', '00000000-0000-0000-0000-0000000000d2',
        '00000000-0000-0000-0000-0000000000b2', '00000000-0000-0000-0000-0000000000b2', 'restricted',
        CURRENT_DATE + 180, true, '00000000-0000-0000-0000-0000000000b2');

INSERT INTO record_search (record_id, visibility, project_id, document)
VALUES ('00000000-0000-0000-0000-000000000f10', 'restricted', '00000000-0000-0000-0000-0000000000d2',
        to_tsvector('english', 'Acme runs on-premise only'));

-- ---------------------------------------------------------------------------
-- Assertions, executed as the application role
-- ---------------------------------------------------------------------------

SET LOCAL ROLE workgraph_app;

DO $$
DECLARE
  alice   text := '00000000-0000-0000-0000-0000000000b1';
  bob     text := '00000000-0000-0000-0000-0000000000b2';
  mallory text := '00000000-0000-0000-0000-0000000000b4';
  n integer;
BEGIN
  -- Alice leads the OSS project only.
  PERFORM set_config('workgraph.user_id', alice, true);

  SELECT count(*) INTO n FROM projects;
  IF n <> 1 THEN RAISE EXCEPTION 'Alice should see exactly 1 project, saw %', n; END IF;

  SELECT count(*) INTO n FROM projects WHERE slug = 'acme-delivery';
  IF n <> 0 THEN RAISE EXCEPTION 'Alice must not see the restricted client project'; END IF;

  SELECT count(*) INTO n FROM work_refs WHERE bead_id = 'acme-1';
  IF n <> 0 THEN RAISE EXCEPTION 'Alice must not see another project''s work items'; END IF;

  -- The knowledge chain must not leak at any layer.
  SELECT count(*) INTO n FROM knowledge_sources;
  IF n <> 0 THEN RAISE EXCEPTION 'Alice must not see restricted source metadata, saw %', n; END IF;

  SELECT count(*) INTO n FROM knowledge_candidates;
  IF n <> 0 THEN RAISE EXCEPTION 'Alice must not see restricted candidate text, saw %', n; END IF;

  SELECT count(*) INTO n FROM knowledge_records;
  IF n <> 0 THEN RAISE EXCEPTION 'Alice must not see restricted records, saw %', n; END IF;

  -- Keyword search must not reach further than a direct read.
  SELECT count(*) INTO n FROM record_search
  WHERE document @@ plainto_tsquery('english', 'Acme');
  IF n <> 0 THEN RAISE EXCEPTION 'restricted record leaked through keyword search'; END IF;

  -- Bob leads the client project and should see his own chain.
  PERFORM set_config('workgraph.user_id', bob, true);

  SELECT count(*) INTO n FROM projects WHERE slug = 'acme-delivery';
  IF n <> 1 THEN RAISE EXCEPTION 'Bob should see his own project'; END IF;

  SELECT count(*) INTO n FROM knowledge_candidates;
  IF n <> 1 THEN RAISE EXCEPTION 'Bob should see his project''s candidate, saw %', n; END IF;

  SELECT count(*) INTO n FROM projects WHERE slug = 'oss-portal';
  IF n <> 0 THEN RAISE EXCEPTION 'Bob must not see the OSS project he is not a member of'; END IF;

  -- Mallory is authenticated but a member of nothing.
  PERFORM set_config('workgraph.user_id', mallory, true);

  SELECT count(*) INTO n FROM projects;
  IF n <> 0 THEN RAISE EXCEPTION 'a non-member must see no projects, saw %', n; END IF;

  -- No session identity at all: the deny-by-default case.
  PERFORM set_config('workgraph.user_id', '', true);

  SELECT count(*) INTO n FROM projects;
  IF n <> 0 THEN RAISE EXCEPTION 'an unidentified session must see nothing, saw %', n; END IF;

  SELECT count(*) INTO n FROM knowledge_records;
  IF n <> 0 THEN RAISE EXCEPTION 'an unidentified session must see no records, saw %', n; END IF;

  -- A member must still be able to write. RLS that blocks legitimate writes
  -- gets disabled in a hurry, which is how these controls die.
  PERFORM set_config('workgraph.user_id', bob, true);
  INSERT INTO knowledge_records (organisation_id, record_type, statement, scope, project_id,
                                 owner_user_id, reviewer_user_id, visibility, review_after,
                                 human_authored, author_user_id)
  VALUES ('00000000-0000-0000-0000-0000000000a1', 'constraint', 'Acme requires EU data residency.',
          'project', '00000000-0000-0000-0000-0000000000d2',
          '00000000-0000-0000-0000-0000000000b2', '00000000-0000-0000-0000-0000000000b2',
          'restricted', CURRENT_DATE + 365, true, '00000000-0000-0000-0000-0000000000b2');

  SELECT count(*) INTO n FROM knowledge_records;
  IF n <> 2 THEN RAISE EXCEPTION 'Bob should now see 2 of his records, saw %', n; END IF;

  -- ...and the new record must still be invisible to Alice.
  PERFORM set_config('workgraph.user_id', alice, true);
  SELECT count(*) INTO n FROM knowledge_records;
  IF n <> 0 THEN RAISE EXCEPTION 'a newly written restricted record leaked to a non-member'; END IF;

  RAISE NOTICE 'RLS isolation: all assertions passed';
END
$$;

RESET ROLE;
ROLLBACK;
