-- system_bead_outcome agrees with system_bead_detail, for every bead.
--
-- The outcome is derived in two places. system_bead_detail answers for one
-- bead and assembles pull requests, spend and a log tail with it, which a list
-- cannot afford per row -- so system_bead_outcome exists for the project page.
--
-- Rewriting the two-hundred-line detail function to call the small one was the
-- alternative, and it was not taken: retyping a long function to change a few
-- lines is exactly how an IS DISTINCT FROM guard was dropped out of the cell
-- registration earlier the same day. Duplication that a test pins is safer
-- than a rewrite that nothing checks.
--
-- So this is that pin. It compares the two for every bead in the database
-- rather than for a fixture, because the interesting cases are the ones real
-- data produces: a bead with two runs, a bead whose work is in a pull request,
-- a bead never dispatched.
--
-- Owner-only by design, recorded in scripts/check_rls_tests.py: both functions
-- are SECURITY DEFINER and the property is that they agree, not who may read
-- them.
\set ON_ERROR_STOP on

BEGIN;

-- A fixture covering every branch, because the sweep below cannot.
--
-- The sweep compares beads that already exist, which is where the interesting
-- cases live -- a bead with two runs, one whose work is in a pull request. On
-- CI's database there are none: the seed migrations create projects and the
-- work_refs inserts all live inside function bodies, so nothing is projected.
-- The sweep's own guard caught that ("no beads to compare, so this test
-- asserted nothing"), which is the guard working and the test being
-- insufficient.
--
-- So both: a fixture that pins all seven outcomes deterministically, and the
-- sweep afterwards for whatever the database really holds.
DO $$
DECLARE
  org uuid; node uuid; cell uuid; u uuid; graph uuid; admin uuid;
  b text; want text; got_out text; got_det text;
BEGIN
  SELECT id INTO org FROM organisations ORDER BY created_at LIMIT 1;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Outcome Requester', 'outcome@example.invalid') RETURNING id INTO u;
  INSERT INTO execution_nodes (hostname, environment)
  VALUES ('outcome-probe.invalid','staging')
  ON CONFLICT (hostname) DO UPDATE SET environment = EXCLUDED.environment RETURNING id INTO node;
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  VALUES (node,'out-cell','wgcell_out','oss') RETURNING id INTO cell;
  INSERT INTO beads_databases (organisation_id, execution_cell_id, name, scope)
  VALUES (org, cell, 'out-graph', 'company') RETURNING id INTO graph;

  -- One bead per outcome. The bead's own status and its latest job's status
  -- are the two inputs, so each row sets both.
  INSERT INTO work_refs (organisation_id, beads_database_id, execution_cell_id,
                         bead_id, title, kind, status)
  VALUES (org, graph, cell, 'oc-never',  'never dispatched', 'task', 'open'),
         (org, graph, cell, 'oc-queued', 'queued',           'task', 'open'),
         (org, graph, cell, 'oc-running','running',          'task', 'open'),
         (org, graph, cell, 'oc-failed', 'failed',           'task', 'open'),
         (org, graph, cell, 'oc-landed', 'in a pull request','task', 'open'),
         (org, graph, cell, 'oc-blocked','ran, no delivery', 'task', 'open'),
         (org, graph, cell, 'oc-done',   'finished',         'task', 'closed');

  INSERT INTO work_queue (kind, cell, rig, bead, status, requested_by)
  VALUES ('work','out-cell','r','oc-queued', 'queued',  u),
         ('work','out-cell','r','oc-running','running', u),
         ('work','out-cell','r','oc-failed', 'failed',  u),
         ('work','out-cell','r','oc-landed', 'done',    u),
         ('work','out-cell','r','oc-blocked','done',    u),
         ('work','out-cell','r','oc-done',   'done',    u);

  -- `landed` is the branch that must come BEFORE the closed check: an open
  -- bead whose work is in a pull request is awaiting review, not stuck.
  -- Reporting that as `blocked` was actively wrong once.
  INSERT INTO bead_pull_requests (bead, execution_cell_id, rig, provider, owner, name,
                                  number, url, head, base)
  VALUES ('oc-landed', cell, 'r', 'github', 'datopian', 'probe', 1,
          'https://github.com/datopian/probe/pull/1', 'bead/oc-landed', 'main');

  SELECT g.user_id INTO admin
    FROM role_grants g
   WHERE g.project_id IS NULL AND g.role_name = 'organisation_admin'
     AND (g.expires_at IS NULL OR g.expires_at > now())
   LIMIT 1;
  IF admin IS NULL THEN
      RAISE EXCEPTION 'no organisation admin to read as';
  END IF;
  PERFORM set_config('workgraph.user_id', admin::text, true);

  FOR b, want IN
      SELECT * FROM (VALUES
        ('oc-never','never_dispatched'), ('oc-queued','queued'),
        ('oc-running','running'), ('oc-failed','failed'),
        ('oc-landed','landed'), ('oc-blocked','blocked'), ('oc-done','done')
      ) AS v(b, want)
  LOOP
      SELECT system_bead_outcome(b) INTO got_out;
      SELECT system_bead_detail(b)->>'outcome' INTO got_det;
      IF got_out <> want THEN
          RAISE EXCEPTION 'system_bead_outcome(%) is %, wanted %', b, got_out, want;
      END IF;
      IF got_det <> want THEN
          RAISE EXCEPTION 'system_bead_detail(%) is %, wanted %', b, got_det, want;
      END IF;
  END LOOP;
END
$$;

ROLLBACK;

DO $$
DECLARE
  r record;
  disagreed integer := 0;
  checked integer := 0;
  admin uuid;
BEGIN
  -- Both functions refuse an unauthenticated caller, so one is set. An
  -- organisation-wide admin, because can_read_project lets those read
  -- everything and the point of this test is to compare EVERY bead: a narrower
  -- caller would compare a subset and call it exhaustive.
  SELECT g.user_id INTO admin
    FROM role_grants g
   WHERE g.project_id IS NULL AND g.role_name = 'organisation_admin'
     AND (g.expires_at IS NULL OR g.expires_at > now())
   LIMIT 1;
  IF admin IS NULL THEN
      RAISE EXCEPTION 'no organisation admin to read as, so nothing could be compared';
  END IF;
  PERFORM set_config('workgraph.user_id', admin::text, true);
  FOR r IN SELECT bead_id FROM work_refs ORDER BY bead_id
  LOOP
    checked := checked + 1;
    IF (SELECT system_bead_outcome(r.bead_id))
       IS DISTINCT FROM (SELECT system_bead_detail(r.bead_id)->>'outcome') THEN
      disagreed := disagreed + 1;
      RAISE WARNING 'bead % : outcome says %, detail says %',
        r.bead_id,
        quote_nullable((SELECT system_bead_outcome(r.bead_id))),
        quote_nullable((SELECT system_bead_detail(r.bead_id)->>'outcome'));
    END IF;
  END LOOP;

  IF disagreed > 0 THEN
    RAISE EXCEPTION 'the two outcome derivations disagree on % of % beads; the '
      'project page and the bead page would tell a reader different things',
      disagreed, checked;
  END IF;

  -- Zero is acceptable HERE, and only because the fixture above has already
  -- pinned all seven outcomes. On CI's database there are no projected beads
  -- at all; on a real one there are hundreds, and comparing them is the part
  -- a fixture cannot do.
  RAISE NOTICE 'swept % existing beads', checked;
END
$$;

SELECT 'the two outcome derivations agree' AS result;
