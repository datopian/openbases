-- Where an agent's work went (the landing path).
--
-- Until now a run that changed code closed its bead with the change stranded
-- in the rig's working tree on the node. sa-kfh is the case: it renamed a hero
-- tab label, closed correctly, and left
--
--     M site/components/home/LandingHero.tsx
--
-- on the execution node with no commit, no branch and no pull request. The
-- agent cannot push -- its tools are Read, Grep, Glob, Edit, Write and
-- `Bash(bd:*)`, deliberately -- so the landing is the machinery's job, and this
-- is where the result of it is recorded.
--
-- A table rather than a column on work_refs, because a bead can be worked more
-- than once: a re-dispatch after a pull request was closed unmerged is an
-- ordinary thing, and the history of where work went is worth keeping.

BEGIN;

CREATE TABLE IF NOT EXISTS bead_pull_requests (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    bead              text NOT NULL,
    execution_cell_id uuid NOT NULL REFERENCES execution_cells(id) ON DELETE CASCADE,
    rig               text NOT NULL,
    provider          text NOT NULL DEFAULT 'github',
    owner             text NOT NULL,
    name              text NOT NULL,
    number            integer NOT NULL,
    url               text NOT NULL,
    head              text NOT NULL,
    base              text NOT NULL,
    job_id            uuid REFERENCES work_queue(id) ON DELETE SET NULL,
    opened_at         timestamptz NOT NULL DEFAULT now(),

    -- One row per branch, which is what makes landing idempotent: a second run
    -- on the same bead pushes to the same branch and finds the pull request it
    -- already opened rather than opening another. Re-dispatching work must not
    -- multiply pull requests -- that is how a review queue becomes noise
    -- nobody reads.
    CONSTRAINT bead_pull_requests_one_per_branch UNIQUE (provider, owner, name, head),

    -- A branch cannot be the base. GitHub refuses it too, but a row that says
    -- a bead's work landed on main would be a lie in the audit trail, and this
    -- table IS the audit trail.
    CONSTRAINT bead_pull_requests_not_the_base CHECK (head <> base)
);

CREATE INDEX IF NOT EXISTS bead_pull_requests_bead ON bead_pull_requests (bead);

ALTER TABLE bead_pull_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE bead_pull_requests FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS bead_pull_requests_read ON bead_pull_requests;

-- Readable by whoever can read the bead. A pull request URL names a repository
-- and a branch, so it says something about work the reader may not be entitled
-- to see -- the same reasoning that governs work_refs, reached through the same
-- project.
CREATE POLICY bead_pull_requests_read ON bead_pull_requests FOR SELECT
    USING (
        current_app_user() IS NOT NULL
        AND EXISTS (
            SELECT 1 FROM work_refs w
             WHERE w.bead_id = bead_pull_requests.bead
               AND (w.project_id IS NULL OR can_read_project(w.project_id))
        )
    );

-- Recording one, idempotently.
--
-- SECURITY DEFINER and cell-scoped: the caller is the node, which has just
-- pushed a branch, and the repository is NOT taken from it. It is looked up
-- from the rig, so a node cannot record a pull request against a repository
-- its rig does not hold.
CREATE OR REPLACE FUNCTION system_record_pull_request(
    p_cell   text,
    p_rig    text,
    p_bead   text,
    p_number integer,
    p_url    text,
    p_head   text,
    p_base   text,
    p_job    uuid DEFAULT NULL
) RETURNS bead_pull_requests
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_cell uuid;
    v_owner text;
    v_name text;
    v_provider text;
    v_row bead_pull_requests;
BEGIN
    SELECT id INTO v_cell FROM execution_cells WHERE slug = p_cell;
    IF v_cell IS NULL THEN
        RAISE EXCEPTION 'no such cell: %', p_cell;
    END IF;

    SELECT provider, owner, name INTO v_provider, v_owner, v_name
      FROM execution_rigs
     WHERE execution_cell_id = v_cell AND rig = p_rig;
    IF v_owner IS NULL OR v_name IS NULL THEN
        RAISE EXCEPTION 'rig % on cell % holds no repository, so nothing can have landed there',
            p_rig, p_cell;
    END IF;

    INSERT INTO bead_pull_requests
        (bead, execution_cell_id, rig, provider, owner, name, number, url, head, base, job_id)
    VALUES
        (p_bead, v_cell, p_rig, v_provider, v_owner, v_name, p_number, p_url, p_head, p_base, p_job)
    ON CONFLICT (provider, owner, name, head) DO UPDATE
        -- The same branch, landed again. The number and URL cannot change for
        -- one branch's open pull request, but the bead and job can: a branch
        -- reused by a later run should say which run put the code there.
        SET bead = EXCLUDED.bead,
            job_id = coalesce(EXCLUDED.job_id, bead_pull_requests.job_id),
            number = EXCLUDED.number,
            url = EXCLUDED.url
    RETURNING * INTO v_row;

    RETURN v_row;
END;
$$;

REVOKE ALL ON FUNCTION system_record_pull_request(text, text, text, integer, text, text, text, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_record_pull_request(text, text, text, integer, text, text, text, uuid) TO workgraph_app;
GRANT SELECT ON bead_pull_requests TO workgraph_app;

-- And the bead's detail carries it, so "where did that work go" is answerable
-- from the interface rather than by reading a node's working tree.
DROP FUNCTION IF EXISTS system_bead_detail(text);

CREATE FUNCTION system_bead_detail(p_bead text)
RETURNS jsonb
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
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
    v_outcome := CASE
        WHEN latest IS NULL OR latest.id IS NULL THEN 'never_dispatched'
        WHEN latest.status IN ('queued', 'running') THEN latest.status
        WHEN latest.status = 'failed' THEN 'failed'
        -- Ran, exited 0, and the bead is still open: the agent did not finish
        -- the work. Usually it says why in a comment, which is carried above.
        WHEN w.status IS DISTINCT FROM 'closed' THEN 'blocked'
        ELSE 'done'
    END;

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
            'newest_record', (SELECT max(occurred_at) FROM usage_records u
                               WHERE u.bead = p_bead)
        )
    ) INTO result;

    RETURN result;
END
$$;

REVOKE ALL ON FUNCTION system_bead_detail(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_bead_detail(text) TO workgraph_app;

COMMIT;
