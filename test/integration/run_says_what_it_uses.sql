-- A run says which harness and model it uses, while it is still running.
--
-- Asked of the MCP surface: the status of a bead in progress, including the
-- model and harness and the cost. Two thirds of that was unanswerable. The
-- model appeared only through usage_records, which the cost importer fills
-- hourly, so a running bead showed none; and the harness was recorded nowhere
-- at all, which stopped being a detail when the default became OpenCode.

BEGIN;

DO $$
DECLARE
  org uuid; node uuid; cell uuid; u uuid; backup uuid; proj uuid;
  job uuid; d jsonb; got text;
BEGIN
  SELECT id INTO org FROM organisations WHERE slug = 'datopian';

  INSERT INTO execution_nodes (hostname, environment)
  VALUES ('plan-probe.invalid', 'staging')
  ON CONFLICT (hostname) DO UPDATE SET environment = EXCLUDED.environment
  RETURNING id INTO node;
  INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
  VALUES (node, 'plan-cell', 'wgcell_plan', 'oss') RETURNING id INTO cell;

  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Plan Owner', 'plan-owner@example.invalid') RETURNING id INTO u;
  INSERT INTO users (organisation_id, display_name, primary_email)
  VALUES (org, 'Plan Backup', 'plan-backup@example.invalid') RETURNING id INTO backup;
  INSERT INTO projects (organisation_id, slug, name, primary_owner_id, backup_owner_id,
                        execution_cell_id)
  VALUES (org, 'plan-proj', 'Planned Project', u, backup, cell) RETURNING id INTO proj;
  INSERT INTO project_memberships (project_id, user_id, role_name)
  VALUES (proj, u, 'project_lead');

  PERFORM system_project_bead('plan-cell', 'pl-1', 'Something in flight', 'task', 'open',
          ARRAY['wg-project-plan-proj']);

  -- A job that is RUNNING, which is the case the question is about.
  INSERT INTO work_queue (kind, cell, rig, bead, status, claimed_at)
  VALUES ('work', 'plan-cell', 'planrig', 'pl-1', 'running', now())
  RETURNING id INTO job;

  PERFORM set_config('workgraph.user_id', u::text, true);

  -- Before the node reports: null rather than a guess. A harness invented from
  -- a default would be worse than none, because it would look like knowledge.
  d := system_bead_detail('pl-1');
  IF d -> 'run' ->> 'harness' IS NOT NULL OR d -> 'run' ->> 'model' IS NOT NULL THEN
    RAISE EXCEPTION 'a run reported a harness before the node said what it uses';
  END IF;

  -- And the spend says its number cannot be trusted yet, rather than showing a
  -- zero that reads as free.
  IF (d -> 'spend' ->> 'awaiting_import')::boolean IS NOT TRUE THEN
    RAISE EXCEPTION 'a running bead with no imported usage does not say it is awaiting import';
  END IF;

  PERFORM system_record_run_plan(job, 'opencode', 'workers-ai/@cf/zai-org/glm-5.3-flash');

  d := system_bead_detail('pl-1');
  IF d -> 'run' ->> 'harness' <> 'opencode' THEN
    RAISE EXCEPTION 'the harness reads % while the bead is still running', d -> 'run' ->> 'harness';
  END IF;
  IF d -> 'run' ->> 'model' <> 'workers-ai/@cf/zai-org/glm-5.3-flash' THEN
    RAISE EXCEPTION 'the model reads %', d -> 'run' ->> 'model';
  END IF;

  -- Whitespace is not a recorded value: it would read as configured in every
  -- test for it and render as a blank in the interface.
  BEGIN
    UPDATE work_queue SET runtime = '  ' WHERE id = job;
    RAISE EXCEPTION 'a whitespace harness was accepted';
  EXCEPTION WHEN check_violation THEN
    NULL;
  END;

  -- Once usage arrives, the spend stops saying it is waiting.
  INSERT INTO usage_records (provider, model, input_tokens, output_tokens, cost_cents,
                             occurred_at, gateway, external_id, cell, rig, bead, role, succeeded)
  VALUES ('workers-ai', '@cf/zai-org/glm-5.3-flash', 1000, 200, 0.4,
          now(), 'workgraph-staging-oss', 'plan-probe-1', 'plan-cell', 'planrig',
          'pl-1', 'polecat', true);

  d := system_bead_detail('pl-1');
  IF (d -> 'spend' ->> 'awaiting_import')::boolean IS NOT FALSE THEN
    RAISE EXCEPTION 'usage has been imported and the spend still says it is waiting';
  END IF;
  IF (d -> 'spend' ->> 'cents')::numeric <> 0.4 THEN
    RAISE EXCEPTION 'the cost reads % rather than 0.4', d -> 'spend' ->> 'cents';
  END IF;

  -- A job the control plane no longer holds is reported as not recorded rather
  -- than raising: a node should not abandon a run over it.
  IF system_record_run_plan('00000000-0000-0000-0000-000000000000'::uuid,
                            'opencode', 'x') IS NOT NULL THEN
    RAISE EXCEPTION 'recording against an unknown job claimed to succeed';
  END IF;

  RAISE NOTICE 'a running bead names its harness and model, and says whether its cost is in yet';
END $$;

ROLLBACK;
