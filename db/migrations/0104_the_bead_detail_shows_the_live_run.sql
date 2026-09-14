-- The bead page can show what a run is doing right now.
--
-- system_bead_detail's run object carried log_tail = right(result, 2000), and
-- result is empty until the run finishes -- so a bead in flight showed its
-- model and status but nothing about what the agent was actually doing. The
-- only way to see that was to ask someone to read the node. The node already
-- sends a heartbeat note every tick ("186 B of output, wrote X 30s ago"); it
-- was stored on work_queue and surfaced in the work LIST but not in the bead
-- DETAIL. This adds it, so /work/<bead> shows the live line and when it was
-- last seen, and the UI can refresh it.
--
-- Same (text) signature, so CREATE OR REPLACE replaces in place; no overload.
BEGIN;

CREATE OR REPLACE FUNCTION public.system_bead_detail(p_bead text)
 RETURNS jsonb
 LANGUAGE plpgsql
 STABLE SECURITY DEFINER
 SET search_path TO 'public', 'pg_temp'
AS $function$
DECLARE
    w        record;
    latest   record;
    v_outcome text;
    result   jsonb;
BEGIN
    SELECT r.*, c.slug AS cell_slug, p.slug AS project_slug
      INTO w
      FROM work_refs r
      LEFT JOIN execution_cells c ON c.id = r.execution_cell_id
      LEFT JOIN projects p ON p.id = r.project_id
     WHERE r.bead_id = p_bead
     ORDER BY r.last_seen_at DESC NULLS LAST
     LIMIT 1;

    IF w IS NULL OR w.bead_id IS NULL THEN
        RETURN NULL;
    END IF;

    -- The same predicate as work_refs_read, spelled out rather than relied
    -- upon: this function is SECURITY DEFINER, so the policy does not apply to
    -- it and the check has to be here. A company-scoped bead (no project) is
    -- readable by any authenticated caller, which is what the policy says.
    IF current_app_user() IS NULL THEN
        RAISE EXCEPTION 'a bead detail needs an authenticated caller';
    END IF;
    IF w.project_id IS NOT NULL AND NOT can_read_project(w.project_id) THEN
        -- Indistinguishable from "no such bead", deliberately. Confirming that
        -- a bead exists in a project the caller cannot see is itself a leak
        -- (ADR-0013).
        RETURN NULL;
    END IF;

    -- The most recent run for this bead.
    SELECT * INTO latest
      FROM work_queue
     WHERE bead = p_bead
     ORDER BY created_at DESC
     LIMIT 1;

    -- The outcome, derived rather than stored.
    --
    -- `done` in work_queue means wg-runner exited 0, which is not the same as
    -- the work being finished: on 4 September three runs exited 0, left their
    -- beads open, and reported that they could not proceed. So the honest
    -- outcome combines the two facts we already have -- did the run succeed,
    -- and is the bead still open.
    --
    -- Derived rather than added as a column because both inputs are already
    -- recorded and a stored copy would be a third thing to keep in step.
    -- One implementation, not two.
    --
    -- This was a copy of the CASE in system_bead_outcome, and the two drifted
    -- the moment one changed: 0100 taught the function that a closed bead
    -- with no pull request is `closed_unlanded`, and this copy went on saying
    -- `done`. The integration test that asserts the two agree is what caught
    -- it -- which is exactly why that test exists.
    v_outcome := system_bead_outcome(p_bead);

    SELECT jsonb_build_object(
        'bead', w.bead_id,
        'title', w.title,
        'kind', w.kind,
        'status', w.status,
        'project', w.project_slug,
        'cell', w.cell_slug,
        'last_seen', w.last_seen_at,

        -- What happened, in one word plus the evidence for it.
        'outcome', v_outcome,
        'run', CASE WHEN latest IS NULL OR latest.id IS NULL THEN NULL ELSE jsonb_build_object(
            'job', latest.id,
            'status', latest.status,
            'rig', latest.rig,
            -- What is actually running it. Recorded when the node starts the
            -- run, because until then nothing in the control plane knew: the
            -- runner picks the harness and model from a catalogue on the node,
            -- and the only later evidence was usage_records, which the cost
            -- importer fills hourly. So a bead in progress showed no model at
            -- all and no harness ever.
            'harness', latest.runtime,
            'model', latest.model,
            'created', latest.created_at,
            'claimed', latest.claimed_at,
            'finished', latest.finished_at,
            -- Trimmed: the full runner log is thousands of lines of transcript
            -- and the useful part is the tail, which carries the failure.
            'log_tail', right(coalesce(latest.result, ''), 2000),
            -- The live line while a run is in flight: the note the node
            -- sends on every heartbeat ("186 B of output, wrote X 30s ago")
            -- and when it last said so. result is empty until the run
            -- finishes, so without this the bead page could show that a
            -- run was in flight but never what it was doing -- which is
            -- exactly what sent people to ask rather than look.
            'heartbeat', nullif(latest.heartbeat_note, ''),
            'heartbeat_at', latest.heartbeat_at
        ) END,

        -- The agent's own words, which is the most useful artefact of a run and
        -- was previously reachable only by reading the graph.
        'comment', CASE WHEN w.last_comment IS NULL THEN NULL ELSE jsonb_build_object(
            'text', w.last_comment,
            'at', w.last_comment_at,
            'by', w.last_comment_by
        ) END,

        -- Where the work went. The question this answers is the one asked of
        -- sa-kfh: "did it land, because I cannot see it in the GitHub repo?"
        -- Before the landing path existed the honest answer was that nothing
        -- an agent wrote could reach a repository at all, and nothing in the
        -- interface said so.
        --
        -- The NEWEST first, and all of them: a bead worked twice has two
        -- branches, and the one that matters is usually the last, but a closed
        -- unmerged pull request is exactly the history somebody needs when
        -- they ask why the work is not in main.
        'pull_requests', coalesce((
            SELECT jsonb_agg(jsonb_build_object(
                       'url', pr.url,
                       'number', pr.number,
                       'repository', pr.owner || '/' || pr.name,
                       'head', pr.head,
                       'base', pr.base,
                       'opened', pr.opened_at
                   -- The number breaks a tie on the timestamp. Two pull
                   -- requests recorded in one transaction share `now()`
                   -- exactly, and then the order is whatever the plan
                   -- happens to give -- so "the newest first" would be true
                   -- most of the time and silently wrong sometimes. GitHub's
                   -- number increases per repository, which makes it a real
                   -- tie-break rather than a stable-looking one.
                   ) ORDER BY pr.opened_at DESC, pr.number DESC)
              FROM bead_pull_requests pr
             WHERE pr.bead = p_bead
        ), '[]'::jsonb),

        -- Which model, and what it cost. Per model rather than a single total,
        -- because "which model was used" is a question people ask and a sum
        -- cannot answer it.
        -- Spend, with the lag stated rather than left to be inferred.
        --
        -- Cost comes from the AI Gateway through an hourly import, so a run
        -- that finished five minutes ago has no cost recorded and a run still
        -- going has none either. A zero that means "not imported yet" and a
        -- zero that means "free" are different answers to "what did that
        -- cost", and a reader cannot tell them apart without this.
        'spend', jsonb_build_object(
            'cents', coalesce((SELECT round(sum(cost_cents), 3) FROM usage_records u
                                WHERE u.bead = p_bead), 0),
            'calls', coalesce((SELECT count(*) FROM usage_records u
                                WHERE u.bead = p_bead), 0),
            'by_model', coalesce((
                SELECT jsonb_agg(m ORDER BY (m->>'cents')::numeric DESC) FROM (
                    SELECT jsonb_build_object(
                        'model', u.model,
                        'provider', u.provider,
                        'calls', count(*),
                        'input_tokens', sum(u.input_tokens),
                        'output_tokens', sum(u.output_tokens),
                        'cents', round(sum(u.cost_cents), 3)
                    ) AS m
                      FROM usage_records u
                     WHERE u.bead = p_bead
                     GROUP BY u.model, u.provider
                ) models), '[]'::jsonb),
            -- Usage arrives from the AI Gateway on an hourly import, so a run
            -- that finished minutes ago shows no spend yet. Saying so beats a
            -- zero that reads as free.
            -- True when this run's usage has not been imported yet: either
            -- nothing is recorded for the bead, or the newest record predates
            -- the run being claimed.
            'awaiting_import', (
                latest.id IS NOT NULL
                AND latest.claimed_at IS NOT NULL
                AND coalesce(
                        (SELECT max(u.occurred_at) FROM usage_records u WHERE u.bead = p_bead),
                        '-infinity'::timestamptz
                    ) < latest.claimed_at
            ),
            'newest_record', (SELECT max(occurred_at) FROM usage_records u
                               WHERE u.bead = p_bead)
        )
    ) INTO result;

    RETURN result;
END
$function$;

COMMIT;
