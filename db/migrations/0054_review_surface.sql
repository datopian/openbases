-- The review side of knowledge candidates (WP-H3, plan sections 4.6 and 14.4),
-- and the row-level security three tables were missing.
BEGIN;

-- knowledge_reviews, source_acl_entries and source_snapshots had RLS OFF and no
-- policies, so any authenticated caller could read them.
--
-- That is not academic for reviews. A rejection reason and an edited statement
-- quote the candidate, which quotes the source -- so a review of a restricted
-- client's meeting carries restricted content in a table anyone could read. The
-- ACL entries name the people who were in that meeting, and the snapshot rows
-- name the evidence. All three follow the source, so all three get the source's
-- rule rather than a new one.
ALTER TABLE knowledge_reviews    ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge_reviews    FORCE ROW LEVEL SECURITY;
ALTER TABLE source_acl_entries   ENABLE ROW LEVEL SECURITY;
ALTER TABLE source_acl_entries   FORCE ROW LEVEL SECURITY;
ALTER TABLE source_snapshots     ENABLE ROW LEVEL SECURITY;
ALTER TABLE source_snapshots     FORCE ROW LEVEL SECURITY;

-- A review is as visible as the candidate it decided.
CREATE POLICY knowledge_reviews_read ON knowledge_reviews FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM knowledge_candidates c
          JOIN knowledge_sources s ON s.id = c.source_id
         WHERE c.id = knowledge_reviews.candidate_id
           AND can_read_source(s.project_id, s.visibility)));

-- Writing one requires the same, and that the reviewer is the caller.
--
-- Recording a decision under somebody else's name is the one thing that would
-- make the audit trail worse than having none, so it is refused by the policy
-- rather than by the handler that happens to be in front of it today.
CREATE POLICY knowledge_reviews_insert ON knowledge_reviews FOR INSERT
    WITH CHECK (
        reviewer_user_id = current_app_user()
        AND EXISTS (
            SELECT 1 FROM knowledge_candidates c
              JOIN knowledge_sources s ON s.id = c.source_id
             WHERE c.id = knowledge_reviews.candidate_id
               AND can_read_source(s.project_id, s.visibility)));

-- Decisions are append-only. An edited decision is a rewritten audit trail.
-- No UPDATE or DELETE policy exists, so both are refused.

CREATE POLICY source_acl_read ON source_acl_entries FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM knowledge_sources s
         WHERE s.id = source_acl_entries.source_id
           AND can_read_source(s.project_id, s.visibility)));

CREATE POLICY source_snapshots_read ON source_snapshots FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM knowledge_sources s
         WHERE s.id = source_snapshots.source_id
           AND can_read_source(s.project_id, s.visibility)));

-- Applying a decision to the candidate.
--
-- A function rather than an UPDATE in the handler, because the decision and the
-- status change must happen together or the queue lies: a review recorded
-- without the status moving leaves the candidate pending and it is reviewed
-- twice, and a status moved without a review recorded loses the reason.
--
-- NOT security definer. The reviewer is a person, RLS should evaluate as them,
-- and the policies above already say who may decide what.
CREATE OR REPLACE FUNCTION review_candidate(
    p_candidate_id uuid,
    p_decision     text,
    p_reason       text DEFAULT NULL,
    p_edited       text DEFAULT NULL
) RETURNS text
LANGUAGE plpgsql
AS $$
DECLARE
    v_status text;
    v_user   uuid := current_app_user();
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

    -- Deciding twice is refused rather than silently overwriting. Two reviewers
    -- reaching the queue together is expected; the second one being told is the
    -- difference between a race and a lost decision.
    IF NOT EXISTS (SELECT 1 FROM knowledge_candidates
                    WHERE id = p_candidate_id AND status = 'pending') THEN
        RAISE EXCEPTION 'candidate % is not pending', p_candidate_id;
    END IF;

    INSERT INTO knowledge_reviews
        (candidate_id, reviewer_user_id, decision, reason, edited_statement)
    VALUES (p_candidate_id, v_user, p_decision, p_reason, p_edited);

    UPDATE knowledge_candidates
       SET status = v_status,
           -- An edit replaces the statement. The original survives on the
           -- review row, which is what makes the correction usable as
           -- evaluation data (plan section 14.8).
           statement = coalesce(NULLIF(p_edited, ''), statement)
     WHERE id = p_candidate_id;

    RETURN v_status;
END
$$;

REVOKE ALL ON FUNCTION review_candidate(uuid, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION review_candidate(uuid, text, text, text) TO workgraph_app;

COMMIT;

-- knowledge.review joins the scopes a personal API token may never carry.
--
-- A reviewer is always a named human (plan section 14.4, and the reason
-- knowledge_reviews.reviewer_user_id references users rather than any actor).
-- A token is held by a program, and a program accepting its own extractor's
-- output into company memory is exactly the loop review exists to break.
--
-- The constraint is the authority; internal/tokens mirrors it so a refusal can
-- name the action instead of surfacing a constraint violation.
BEGIN;

ALTER TABLE api_tokens DROP CONSTRAINT IF EXISTS api_tokens_scopes_check;

ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_scopes_check
    CHECK (NOT (scopes && ARRAY[
        'approval.decide',
        'pull_request.merge',
        'deployment.execute',
        'secret.manage',
        'policy.manage',
        'marketing.publish',
        'knowledge.classification.downgrade',
        'knowledge.review'
    ]::text[]));

COMMIT;
