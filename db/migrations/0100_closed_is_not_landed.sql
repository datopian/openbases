-- A closed bead that landed nothing says so.
--
-- Reported after sa-iyu: the bead read `outcome: done` with `pull_requests:
-- []` while 29 files -- a complete, checksummed, R2-verified dataset pack --
-- sat untracked in the cell's working directory and had never reached git.
-- Three re-dispatches re-verified them and landed nothing. Every surface said
-- done. Nothing said "and nothing came of it".
--
-- 0099 derived the outcome from two facts and stopped one short: did the run
-- succeed, and is the bead still open. A third fact was already sitting in
-- the same function -- v_landed, whether a pull request exists -- and was
-- consulted only for OPEN beads, to tell `landed` from `blocked`. For closed
-- beads it was ignored, so "closed" alone became "done".
--
-- So `done` now means closed AND something reached the repository, and
-- `closed_unlanded` is the new word for closed with nothing.
--
-- Deliberately NOT called a failure. A bead whose work needed no code change
-- is the ordinary case, not a fault -- sa-4yn found the accessible name came
-- from the visible text, so there was no attribute to update, and reporting
-- that as broken would be its own lie. `closed_unlanded` states the fact and
-- lets the reader judge: for sa-4yn it is correct and uninteresting, and for
-- sa-iyu it is the whole story.
--
-- The rest of the CASE is unchanged, on purpose. An earlier change to a
-- registration function in this repository rewrote a body instead of editing
-- it and silently dropped a guard; the four branches above the last one are
-- copied verbatim.
BEGIN;

CREATE OR REPLACE FUNCTION system_bead_outcome(p_bead text)
RETURNS text
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_run    text;
    v_bead   text;
    v_landed boolean;
BEGIN
    -- Requires an authenticated caller, like system_bead_detail. Kept from
    -- 0099: the two functions answer the same question, and one refusing an
    -- unauthenticated caller while the other answers is the kind of
    -- difference that becomes a way in later.
    IF current_app_user() IS NULL THEN
        RAISE EXCEPTION 'a bead outcome needs an authenticated caller';
    END IF;

    SELECT status INTO v_run
      FROM work_queue WHERE bead = p_bead ORDER BY created_at DESC LIMIT 1;
    SELECT status INTO v_bead
      FROM work_refs WHERE bead_id = p_bead LIMIT 1;
    SELECT EXISTS (SELECT 1 FROM bead_pull_requests WHERE bead = p_bead)
      INTO v_landed;

    RETURN CASE
        WHEN v_run IS NULL THEN 'never_dispatched'
        WHEN v_run IN ('queued', 'running') THEN v_run
        WHEN v_run = 'failed' THEN 'failed'
        WHEN v_bead IS DISTINCT FROM 'closed' AND v_landed THEN 'landed'
        WHEN v_bead IS DISTINCT FROM 'closed' THEN 'blocked'
        -- Closed. The question 0099 did not ask.
        WHEN v_landed THEN 'done'
        ELSE 'closed_unlanded'
    END;
END
$$;

REVOKE ALL ON FUNCTION system_bead_outcome(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_bead_outcome(text) TO workgraph_app;

-- And system_bead_detail asks the function rather than keeping a copy.
--
-- It carried the same CASE, and the copy is how the two drifted: the moment
-- the function learned about `closed_unlanded`, detail went on reporting
-- `done` for the same bead. The integration test asserting the two agree
-- caught it within a minute of the change -- which is what that test is for,
-- and why the fix is to delete the duplicate rather than update both.
--
-- The rest of this definition is the live one, taken from
-- pg_get_functiondef and edited in exactly one place. Rewriting it by hand
-- was the alternative, and this repository has already lost a guard that way.
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
            'log_tail', right(coalesce(latest.result, ''), 2000)
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
