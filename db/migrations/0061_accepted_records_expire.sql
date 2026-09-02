-- An accepted fact, constraint or assumption gets its freshness date (WP-H4).
--
-- knowledge_records has a CHECK that those three types name a review date --
-- 0002 put it there because a stale fact served as current is the failure mode
-- that makes durable memory worse than no memory. 0056 published records
-- without one, so accepting a fact, a constraint or an assumption did not
-- publish anything: it raised a constraint violation at the reviewer.
--
-- The test only ever accepted a decision, which is the one durable type with no
-- review date, so nothing caught it. It now accepts one of each.
--
-- The horizons are the ones policies/knowledge-review.yaml declares in
-- company-workgraph: fact 180 days, constraint 365, assumption 90, and null for
-- the types that are superseded rather than expiring.
BEGIN;

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
    v_review_after date;
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

            -- The freshness horizon, from policies/knowledge-review.yaml in
            -- company-workgraph: fact 180 days, constraint 365, assumption 90.
            -- A decision or a lesson does not expire -- it is superseded --
            -- and neither does a preference.
            --
            -- Required rather than nice: knowledge_records has a CHECK that a
            -- fact, constraint or assumption names a review date, because a
            -- stale fact served as current is the failure mode. Without this
            -- the insert below is REFUSED, so accepting a fact returned a
            -- constraint violation to the reviewer.
            v_review_after := CASE v_cand.candidate_type
                WHEN 'fact'       THEN current_date + 180
                WHEN 'constraint' THEN current_date + 365
                WHEN 'assumption' THEN current_date + 90
                ELSE NULL END;

            INSERT INTO knowledge_records (
                organisation_id, record_type, statement, scope, project_id,
                owner_user_id, reviewer_user_id, visibility, confidence, review_after,
                human_authored, status
            ) VALUES (
                v_org, v_cand.candidate_type, v_cand.statement, v_scope,
                v_cand.proposed_project_id, v_cand.proposed_owner_user_id,
                v_user, v_cand.visibility, v_cand.confidence, v_review_after,
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
