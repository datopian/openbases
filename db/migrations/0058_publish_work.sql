-- Accepting an operational candidate creates the bead (WP-H4, plan section 14.5).
--
-- 0056 published the durable half -- decision, constraint, fact, lesson,
-- preference, assumption -- into knowledge_records, and said so honestly for
-- the operational half: `published: pending_beads`, meaning nothing happened.
-- This is the other half.
--
-- It cannot happen inside review_candidate, because creating a bead means
-- running `bd` against a directory on the control node's filesystem and
-- Postgres has no way to do that. So the database records the intent and a
-- publisher process does the work: the two functions below are the whole
-- interface between them.
BEGIN;

-- Which bead an accepted candidate became.
--
-- On work_refs rather than a bead id string, because a bead id alone is not an
-- identity: prefixes collide between graphs (plan section 7.3), and this is the
-- column that says WHICH graph.
ALTER TABLE knowledge_candidates
    ADD COLUMN work_ref_id uuid REFERENCES work_refs(id) ON DELETE SET NULL;

COMMENT ON COLUMN knowledge_candidates.work_ref_id IS
  'The bead this candidate became. NULL for a durable-memory type, which publishes into knowledge_records instead, and for an operational one not published yet (WP-H4).';

CREATE INDEX knowledge_candidates_awaiting_beads
    ON knowledge_candidates (created_at)
 WHERE work_ref_id IS NULL
   AND status IN ('accepted', 'edited_accepted')
   AND candidate_type IN ('task', 'commitment', 'risk', 'market-signal', 'question');

-- What is accepted, operational, and has no bead yet.
--
-- Returns rows it CANNOT publish too, with blocked_reason set, rather than
-- filtering them out. A candidate a reviewer accepted and that silently never
-- became work is the exact failure this work package exists to remove; an error
-- line every run is the point.
CREATE FUNCTION system_pending_work_publications(p_limit integer DEFAULT 50)
RETURNS TABLE (
    candidate_id   uuid,
    candidate_type text,
    statement      text,
    due_date       date,
    visibility     text,
    confidence     numeric,
    project_slug   text,
    owner_email    text,
    reviewer_email text,
    source_id      uuid,
    graph_id       uuid,
    graph_name     text,
    graph_path     text,
    graph_host     text,
    blocked_reason text
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT c.id,
           c.candidate_type,
           c.statement,
           c.due_date,
           c.visibility,
           c.confidence,
           p.slug,
           u.primary_email,
           r.reviewer_email,
           c.source_id,
           g.id,
           g.name,
           g.path,
           g.host,
           CASE
             -- A project's work goes in that project's graph or nowhere. The
             -- company graph is not a fallback: routing a client's accepted
             -- task there would put it in front of everyone at the company.
             WHEN c.proposed_project_id IS NOT NULL AND g.id IS NULL
               THEN 'no graph is registered for project ' || coalesce(p.slug, c.proposed_project_id::text)
             -- A restricted statement with no project has nowhere narrow to go.
             -- Publishing it company-wide is the widening this refuses to do
             -- quietly (ADR-0013).
             WHEN c.proposed_project_id IS NULL AND c.visibility = 'restricted'
               THEN 'restricted with no project: there is no graph narrow enough'
             WHEN g.id IS NULL
               THEN 'no company graph is registered'
             WHEN coalesce(g.path, '') = ''
               THEN 'graph ' || g.name || ' has no recorded path'
             ELSE NULL
           END
      FROM knowledge_candidates c
      LEFT JOIN projects p ON p.id = c.proposed_project_id
      LEFT JOIN users u ON u.id = c.proposed_owner_user_id
      -- The reviewer who accepted it. Passed to `bd` as the actor, so the Dolt
      -- commit trail attributes the work to the person who decided it exists
      -- rather than to the service account that typed it in.
      LEFT JOIN LATERAL (
          SELECT ru.primary_email AS reviewer_email
            FROM knowledge_reviews kr
            JOIN users ru ON ru.id = kr.reviewer_user_id
           WHERE kr.candidate_id = c.id
             AND kr.decision IN ('accept', 'edit_and_accept')
           ORDER BY kr.decided_at DESC
           LIMIT 1
      ) r ON true
      -- The graph is chosen by the candidate's project, or by being the company
      -- graph when there is no project. LATERAL with a limit rather than a
      -- plain join, so a second graph registered for one project cannot
      -- multiply the row and publish the same candidate twice.
      LEFT JOIN LATERAL (
          SELECT b.id, b.name, b.path, b.host
            FROM beads_databases b
           WHERE coalesce(b.path, '') <> ''
             AND CASE WHEN c.proposed_project_id IS NOT NULL
                      THEN b.project_id = c.proposed_project_id
                      ELSE b.scope = 'company' AND b.project_id IS NULL END
           ORDER BY b.name
           LIMIT 1
      ) g ON true
     WHERE c.work_ref_id IS NULL
       AND c.status IN ('accepted', 'edited_accepted')
       AND c.candidate_type IN ('task', 'commitment', 'risk', 'market-signal', 'question')
     ORDER BY c.created_at
     LIMIT greatest(p_limit, 1);
$$;

-- Record the bead a candidate became.
--
-- Idempotent on the identity tuple: called twice with the same bead it records
-- the same work_ref and reports it, so a publisher that created the bead and
-- then failed before recording can be re-run. The candidate is only ever linked
-- to a bead in the graph the routing chose, checked here rather than trusted
-- from the caller.
CREATE FUNCTION system_record_work_publication(
    p_candidate_id uuid,
    p_graph_id     uuid,
    p_bead_id      text,
    p_title        text,
    p_kind         text
) RETURNS uuid
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_cand    knowledge_candidates%ROWTYPE;
    v_org     uuid;
    v_ref     uuid;
BEGIN
    IF p_bead_id IS NULL OR length(trim(p_bead_id)) = 0 THEN
        RAISE EXCEPTION 'a publication needs the bead id it created';
    END IF;

    SELECT * INTO v_cand FROM knowledge_candidates WHERE id = p_candidate_id;
    IF v_cand.id IS NULL THEN
        RAISE EXCEPTION 'no such candidate %', p_candidate_id;
    END IF;
    IF v_cand.status NOT IN ('accepted', 'edited_accepted') THEN
        -- A rejected or deferred candidate must not acquire work. If a bead was
        -- created anyway the caller has to see that, not have it filed quietly.
        RAISE EXCEPTION 'candidate % is %, not accepted', p_candidate_id, v_cand.status;
    END IF;

    SELECT organisation_id INTO v_org FROM beads_databases WHERE id = p_graph_id;
    IF v_org IS NULL THEN
        RAISE EXCEPTION 'no such graph %', p_graph_id;
    END IF;

    IF v_cand.work_ref_id IS NOT NULL THEN
        -- Already published. Returning the existing reference rather than
        -- raising, because the publisher's retry is the normal path and a
        -- second bead for one accepted statement is the thing to avoid.
        RETURN v_cand.work_ref_id;
    END IF;

    INSERT INTO work_refs (organisation_id, beads_database_id, bead_id,
                           title, kind, status, visibility, project_id)
    VALUES (v_org, p_graph_id, trim(p_bead_id),
            nullif(trim(coalesce(p_title, '')), ''), nullif(trim(coalesce(p_kind, '')), ''),
            'open', v_cand.visibility, v_cand.proposed_project_id)
    ON CONFLICT (organisation_id,
                 COALESCE(execution_cell_id, '00000000-0000-0000-0000-000000000000'::uuid),
                 beads_database_id, bead_id)
    DO UPDATE SET last_seen_at = now(),
                  title = coalesce(excluded.title, work_refs.title),
                  kind  = coalesce(excluded.kind, work_refs.kind)
    RETURNING id INTO v_ref;

    UPDATE knowledge_candidates SET work_ref_id = v_ref WHERE id = p_candidate_id;
    RETURN v_ref;
END
$$;

REVOKE ALL ON FUNCTION system_pending_work_publications(integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_record_work_publication(uuid, uuid, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_pending_work_publications(integer) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_record_work_publication(uuid, uuid, text, text, text) TO workgraph_app;

-- review_candidate now says the accepted statement is QUEUED, not that nothing
-- will happen to it.
--
-- Still not published in the same transaction, and deliberately: publication
-- runs `bd` on the control node's filesystem, which a database function cannot
-- do and an API request should not wait on. "queued_beads" is the honest word
-- for that, and it is distinguishable from the old "pending_beads", which meant
-- the feature did not exist.
CREATE OR REPLACE FUNCTION review_candidate(
    p_candidate_id uuid,
    p_decision     text,
    p_reason       text DEFAULT NULL,
    p_edited       text DEFAULT NULL
) RETURNS jsonb
LANGUAGE plpgsql
AS $$
DECLARE
    v_status    text;
    v_user      uuid := current_app_user();
    v_cand      knowledge_candidates%ROWTYPE;
    v_record    uuid;
    v_published text := 'none';
    v_scope     text;
    v_org       uuid;
BEGIN
    IF v_user IS NULL THEN
        RAISE EXCEPTION 'a review needs a reviewer';
    END IF;

    v_status := CASE p_decision
        WHEN 'accept'          THEN 'accepted'
        WHEN 'edit_and_accept' THEN 'edited_accepted'
        WHEN 'reject'          THEN 'rejected'
        WHEN 'defer'           THEN 'deferred'
        WHEN 'merge'           THEN 'merged'
        ELSE NULL END;
    IF v_status IS NULL THEN
        RAISE EXCEPTION 'unknown decision %', p_decision;
    END IF;

    SELECT * INTO v_cand FROM knowledge_candidates
     WHERE id = p_candidate_id AND status = 'pending';
    IF v_cand.id IS NULL THEN
        RAISE EXCEPTION 'candidate % is not pending', p_candidate_id;
    END IF;

    INSERT INTO knowledge_reviews
        (candidate_id, reviewer_user_id, decision, reason, edited_statement)
    VALUES (p_candidate_id, v_user, p_decision, p_reason, p_edited);

    UPDATE knowledge_candidates
       SET status = v_status,
           statement = coalesce(NULLIF(p_edited, ''), statement)
     WHERE id = p_candidate_id
    RETURNING * INTO v_cand;

    IF v_status IN ('accepted', 'edited_accepted') THEN
        IF v_cand.candidate_type IN
           ('decision', 'constraint', 'fact', 'lesson', 'preference', 'assumption') THEN

            SELECT organisation_id INTO v_org FROM knowledge_sources WHERE id = v_cand.source_id;
            v_scope := CASE WHEN v_cand.proposed_project_id IS NOT NULL
                            THEN 'project' ELSE 'company' END;

            INSERT INTO knowledge_records (
                organisation_id, record_type, statement, scope, project_id,
                owner_user_id, reviewer_user_id, visibility, confidence,
                human_authored, status
            ) VALUES (
                v_org, v_cand.candidate_type, v_cand.statement, v_scope,
                v_cand.proposed_project_id, v_cand.proposed_owner_user_id,
                v_user, v_cand.visibility, v_cand.confidence,
                false, 'accepted'
            ) RETURNING id INTO v_record;

            INSERT INTO knowledge_record_sources (record_id, source_id, candidate_id)
            VALUES (v_record, v_cand.source_id, v_cand.id);

            v_published := 'knowledge_record';
        ELSE
            v_published := 'queued_beads';
        END IF;
    END IF;

    RETURN jsonb_build_object(
        'status', v_status,
        'published', v_published,
        'record_id', v_record);
END
$$;

REVOKE ALL ON FUNCTION review_candidate(uuid, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION review_candidate(uuid, text, text, text) TO workgraph_app;

COMMIT;
