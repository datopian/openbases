-- Accepting a candidate publishes it (WP-H4, plan section 14.5).
--
-- Until now `accept` moved a status and nothing else happened, which made the
-- review queue a place where decisions went to be marked as read.
--
-- The routing is already encoded in the schema and is not a new decision here:
-- knowledge_records.record_type is exactly {decision, constraint, fact, lesson,
-- preference, assumption} -- the durable-memory types -- while task,
-- commitment, risk, market-signal and question are operational and belong in
-- Beads. This publishes the first set. The Beads half follows, and until it
-- lands an accepted task says so rather than pretending.
BEGIN;

-- review_candidate gains a return shape, so a caller learns what publication
-- did rather than having to go looking.
DROP FUNCTION IF EXISTS review_candidate(uuid, text, text, text);

CREATE FUNCTION review_candidate(
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
            -- A record scoped to a project must name one; without a project it
            -- is company-wide. The constraint enforces the pairing, and
            -- choosing here rather than defaulting keeps the two in step.
            v_scope := CASE WHEN v_cand.proposed_project_id IS NOT NULL
                            THEN 'project' ELSE 'company' END;

            INSERT INTO knowledge_records (
                organisation_id, record_type, statement, scope, project_id,
                owner_user_id, reviewer_user_id, visibility, confidence,
                -- The reviewer is the author of the acceptance, not of the
                -- statement, so human_authored stays false: the text came from
                -- an extractor and the provenance below says which source.
                human_authored, status
            ) VALUES (
                v_org, v_cand.candidate_type, v_cand.statement, v_scope,
                v_cand.proposed_project_id, v_cand.proposed_owner_user_id,
                v_user, v_cand.visibility, v_cand.confidence,
                false, 'accepted'
            ) RETURNING id INTO v_record;

            -- Provenance, in the same transaction. The trigger enforcing it is
            -- DEFERRABLE INITIALLY DEFERRED precisely so the record and its
            -- sources can be written together; a record without this would fail
            -- at commit rather than be published unprovenanced.
            INSERT INTO knowledge_record_sources (record_id, source_id, candidate_id)
            VALUES (v_record, v_cand.source_id, v_cand.id);

            v_published := 'knowledge_record';
        ELSE
            -- Operational. Beads publication is WP-H4's second half; saying so
            -- is better than a silent no-op, because "accepted" currently reads
            -- as "this became work" and it has not.
            v_published := 'pending_beads';
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

-- knowledge_records and its provenance need INSERT policies, or the publication
-- above is refused for the reviewer it is meant to run as.
--
-- Scoped to what the reviewer may already see: a record may only be written
-- into a project whose material they can read, and its provenance may only name
-- a source they can read. Without the second, an accepted record could cite a
-- source the writer has no access to, which would launder a restricted source
-- into a record somebody else may read.
CREATE POLICY knowledge_records_insert ON knowledge_records FOR INSERT
    WITH CHECK (
        reviewer_user_id = current_app_user()
        AND (project_id IS NULL OR can_read_project(project_id)));

CREATE POLICY knowledge_record_sources_insert ON knowledge_record_sources FOR INSERT
    WITH CHECK (EXISTS (
        SELECT 1 FROM knowledge_sources s
         WHERE s.id = knowledge_record_sources.source_id
           AND can_read_source(s.project_id, s.visibility)));

COMMIT;
