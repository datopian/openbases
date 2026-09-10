-- What happened to a bead, and who may ask (wg-m07).
--
-- Three beads were dispatched on 4 September. work_queue said `done` for all
-- three because wg-runner exited 0, and all three had in fact reported that
-- they could not do the work: the agent left them open and wrote a comment
-- saying what it had looked for and not found.
--
-- So `outcome` is DERIVED from two facts already recorded -- did the run
-- succeed, and is the bead still open -- rather than trusting either alone.
-- Every case below is one of those combinations, because the whole value of
-- the field is telling them apart.

BEGIN;

DO $$
DECLARE
  org uuid; node uuid; cell uuid; u uuid; backup uuid; outsider uuid;
  proj uuid; other uuid; d jsonb;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  INSERT INTO execution_nodes (hostname, environment)
  VALUES ('detail-probe.invalid', 'staging')
  ON CONFLICT (hostname) DO UPDATE SET environment = EXCLUDED.environment
  RETURNING id INTO node;
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  VALUES (node, 'detail-cell', 'wgcell_detail', 'oss')
  RETURNING id INTO cell;

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Detail Owner', 'detail-owner@example.invalid') RETURNING id INTO u;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Detail Backup', 'detail-backup@example.invalid') RETURNING id INTO backup;
  -- No grants at all. Every earlier attempt at an "outsider" in this suite
  -- turned out to hold a company-wide role, which made the assertion vacuous.
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Detail Outsider', 'detail-outsider@example.invalid') RETURNING id INTO outsider;

  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id,
                        execution_cell_id)
  VALUES (org, 'detail-proj', 'Detail Project', u, backup, cell) RETURNING id INTO proj;
  INSERT INTO project_memberships (project_id, user_id, role_name)
  VALUES (proj, u, 'project_lead'), (proj, backup, 'backup_operator');

  -- A second project on the same cell, so attribution comes from the label
  -- rather than the cell, and so the cell is shared as it is in reality.
  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id,
                        execution_cell_id)
  VALUES (org, 'detail-other', 'Detail Other', u, backup, cell) RETURNING id INTO other;

  -- Four beads, one per outcome that needs telling apart.
  PERFORM system_project_bead('detail-cell', 'dt-done', 'Finished', 'task', 'closed',
                              ARRAY['wg-project-detail-proj']);
  PERFORM system_project_bead('detail-cell', 'dt-blocked', 'Could not', 'task', 'open',
                              ARRAY['wg-project-detail-proj'],
                              'No PortalJS source in this environment. Leaving this open.',
                              now(), 'workgraph-oss');
  PERFORM system_project_bead('detail-cell', 'dt-failed', 'Crashed', 'task', 'open',
                              ARRAY['wg-project-detail-proj']);
  PERFORM system_project_bead('detail-cell', 'dt-fresh', 'Never run', 'task', 'open',
                              ARRAY['wg-project-detail-proj']);
  -- Closed, ran successfully, and nothing reached the repository. sa-iyu was
  -- exactly this and reported `done` while 29 finished files sat uncommitted
  -- on a node.
  PERFORM system_project_bead('detail-cell', 'dt-unlanded', 'Closed, nothing landed',
                              'task', 'closed', ARRAY['wg-project-detail-proj']);

  INSERT INTO work_queue (kind, cell, rig, bead, status, project_id, finished_at, result)
  VALUES ('work', 'detail-cell', 'sandbox', 'dt-done', 'done', proj, now(), 'ok'),
         ('work', 'detail-cell', 'sandbox', 'dt-blocked', 'done', proj, now(), 'ran, did nothing'),
         ('work', 'detail-cell', 'sandbox', 'dt-failed', 'failed', proj, now(), 'boom'),
         ('work', 'detail-cell', 'sandbox', 'dt-unlanded', 'done', proj, now(), 'ok');

  -- `done` requires a pull request as well as a closed bead, so dt-done needs
  -- one. Without it dt-done and dt-unlanded are the same row set, which is
  -- precisely the confusion this distinction removes.
  INSERT INTO bead_pull_requests (bead, execution_cell_id, rig, provider, owner, name,
                                  number, url, head, base)
  VALUES ('dt-done', cell, 'sandbox', 'github', 'datopian', 'probe', 1,
          'https://github.com/datopian/probe/pull/1', 'bead/dt-done', 'main');

  PERFORM set_config('workgraph.user_id', u::text, true);

  -- A run that succeeded, closed the bead, AND landed something is `done`.
  d := system_bead_detail('dt-done');
  IF d->>'outcome' <> 'done' THEN
    RAISE EXCEPTION 'a closed bead whose work landed is %, want done', d->>'outcome';
  END IF;

  -- Closed, successful, and nothing in the repository. Not a failure -- a bead
  -- needing no code change looks like this -- but it must not read as `done`,
  -- which is what let sa-iyu report success with its work uncommitted.
  d := system_bead_detail('dt-unlanded');
  IF d->>'outcome' <> 'closed_unlanded' THEN
    RAISE EXCEPTION 'a closed bead that landed nothing is %, want closed_unlanded',
      d->>'outcome';
  END IF;

  -- THE CASE THIS EXISTS FOR. The run exited 0 and the bead is still open, so
  -- the work did not get done. Reporting this as `done` is what made three real
  -- runs look like successes.
  d := system_bead_detail('dt-blocked');
  IF d->>'outcome' <> 'blocked' THEN
    RAISE EXCEPTION 'a successful run that left the bead open is %, want blocked', d->>'outcome';
  END IF;
  IF d->'comment'->>'text' NOT LIKE '%Leaving this open%' THEN
    RAISE EXCEPTION 'the agent''s comment did not survive: %', d->'comment';
  END IF;
  IF d->'comment'->>'by' <> 'workgraph-oss' THEN
    RAISE EXCEPTION 'the comment author was lost: %', d->'comment';
  END IF;

  -- A failed run is failed whatever the bead says.
  d := system_bead_detail('dt-failed');
  IF d->>'outcome' <> 'failed' THEN
    RAISE EXCEPTION 'a failed run is %, want failed', d->>'outcome';
  END IF;

  -- Never dispatched is its own answer, not `blocked`. A bead nobody has run is
  -- not a bead that refused.
  d := system_bead_detail('dt-fresh');
  IF d->>'outcome' <> 'never_dispatched' THEN
    RAISE EXCEPTION 'an undispatched bead is %, want never_dispatched', d->>'outcome';
  END IF;
  IF d->'run' <> 'null'::jsonb AND d->'run' IS NOT NULL THEN
    RAISE EXCEPTION 'an undispatched bead reported a run: %', d->'run';
  END IF;

  -- Spend, per model, because "which model" is a question a total cannot answer.
  INSERT INTO usage_records (project_id, provider, model, input_tokens, output_tokens,
                             cost_cents, occurred_at, gateway, external_id, cached,
                             succeeded, role, cell, rig, bead)
  VALUES (proj, 'anthropic', 'anthropic/claude-sonnet-5', 800, 5000, 52.191, now(),
          'g', 'dt-u1', false, true, 'polecat', 'detail-cell', 'sandbox', 'dt-blocked'),
         (proj, 'anthropic', 'anthropic/claude-haiku-4-5', 100, 200, 0.5, now(),
          'g', 'dt-u2', false, true, 'polecat', 'detail-cell', 'sandbox', 'dt-blocked');

  d := system_bead_detail('dt-blocked');
  IF (d->'spend'->>'calls')::int <> 2 THEN
    RAISE EXCEPTION 'spend reported % calls, want 2', d->'spend'->>'calls';
  END IF;
  IF jsonb_array_length(d->'spend'->'by_model') <> 2 THEN
    RAISE EXCEPTION 'two models were used and % are reported',
      jsonb_array_length(d->'spend'->'by_model');
  END IF;
  -- Ordered by cost, so the expensive one is first and a reader sees what
  -- dominated the bill rather than an alphabetical list.
  IF d->'spend'->'by_model'->0->>'model' <> 'anthropic/claude-sonnet-5' THEN
    RAISE EXCEPTION 'the models are not ordered by cost: %', d->'spend'->'by_model';
  END IF;

  RAISE NOTICE 'bead detail: done, blocked, failed and never_dispatched all told apart';
END $$;

-- ---------------------------------------------------------------------------
-- Who may ask
-- ---------------------------------------------------------------------------
--
-- system_bead_detail is SECURITY DEFINER, so the work_refs policy does NOT
-- apply to it and the check has to be inside. A bead in a project the caller
-- cannot see must be answered exactly like a bead that does not exist:
-- confirming it exists leaks the project (ADR-0013).

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

DO $$
DECLARE d jsonb; member uuid; outsider uuid;
BEGIN
  SELECT id INTO member FROM users WHERE primary_email = 'detail-owner@example.invalid';
  SELECT id INTO outsider FROM users WHERE primary_email = 'detail-outsider@example.invalid';

  PERFORM set_config('workgraph.user_id', member::text, true);
  d := system_bead_detail('dt-blocked');
  IF d IS NULL THEN
    RAISE EXCEPTION 'a project member cannot read a bead in their own project';
  END IF;

  PERFORM set_config('workgraph.user_id', outsider::text, true);
  d := system_bead_detail('dt-blocked');
  IF d IS NOT NULL THEN
    RAISE EXCEPTION 'a non-member read a bead in a project they cannot see: %', d->>'bead';
  END IF;

  -- And with no identity at all it raises rather than answering, because a
  -- SECURITY DEFINER function queried as nobody would otherwise return
  -- everything.
  PERFORM set_config('workgraph.user_id', '', true);
  BEGIN
    d := system_bead_detail('dt-blocked');
    RAISE EXCEPTION 'an unauthenticated caller got a bead detail';
  EXCEPTION WHEN others THEN
    IF position('authenticated caller' IN SQLERRM) = 0 THEN RAISE; END IF;
  END;

  RAISE NOTICE 'bead detail visibility: member yes, non-member indistinguishable from absent';
END $$;

RESET ROLE;

ROLLBACK;
