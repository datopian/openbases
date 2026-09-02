-- A reviewer can correct a candidate, not only decide on it (WP-H3).
--
-- WP-H3 asks for a reviewer who can "accept, edit, reject, defer, reclassify
-- and assign owner or project". review_candidate did the first four and none of
-- the last three: a reviewer looking at a task the extractor typed as a
-- question, or a commitment with nobody's name on it, had to reject it and
-- wait for it to be extracted again -- which never happens, because the
-- transcript has already been ingested at that revision.
--
-- So the queue's only answer to "this is nearly right" was to throw it away.
-- That also poisons the evaluation data WP-H6 reads: a rejection that meant
-- "wrong type" is indistinguishable from one that meant "not true".
--
-- Three deliberate limits.
--
-- Reclassifying is TIGHTENING ONLY. Widening a classification is a protected
-- action -- knowledge.classification.downgrade is an ungrantable API scope
-- precisely so it cannot happen through a token -- and the review surface must
-- not be the back door to it.
--
-- Assigning a project is checked with can_read_project, the same predicate
-- every read uses. Assigning into a project the reviewer cannot read would put
-- a statement somewhere they cannot check it again.
--
-- Clearing is separate from setting, because NULL already means "leave this
-- alone". Without p_clear_owner there is no way to say "nobody owns this" that
-- is distinguishable from saying nothing.
BEGIN;

-- Dropped and recreated: the new arguments have defaults, so the four-argument
-- call still resolves, but CREATE OR REPLACE cannot add parameters.
DROP FUNCTION IF EXISTS review_candidate(uuid, text, text, text);

CREATE OR REPLACE FUNCTION review_candidate(
    p_candidate_id uuid,
    p_decision     text,
    p_reason       text DEFAULT NULL,
    p_edited       text DEFAULT NULL,
    -- What the reviewer is correcting, alongside the decision. All optional:
    -- NULL means "leave it", which is why an empty string cannot be used as
    -- the sentinel for "clear it" either -- see p_clear_owner below.
    p_type         text DEFAULT NULL,
    p_owner_email  text DEFAULT NULL,
    p_project      text DEFAULT NULL,
    p_visibility   text DEFAULT NULL,
    -- Clearing is separate from setting, because NULL already means "leave
    -- it". A reviewer who wants an unowned candidate has to say so.
    p_clear_owner  boolean DEFAULT false,
    p_clear_project boolean DEFAULT false
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
    v_type      text;
    v_owner     uuid;
    v_project   uuid;
    v_visibility text;
    v_old_visibility text;
    v_old_type  text;
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

    -- The corrections, resolved and checked BEFORE the decision is recorded,
    -- so a review that cannot be applied is not recorded as having happened.
    v_type       := coalesce(nullif(trim(coalesce(p_type, '')), ''), v_cand.candidate_type);
    v_visibility := coalesce(nullif(trim(coalesce(p_visibility, '')), ''), v_cand.visibility);
    v_old_visibility := v_cand.visibility;
    v_old_type := v_cand.candidate_type;
    v_owner      := v_cand.proposed_owner_user_id;
    v_project    := v_cand.proposed_project_id;

    IF p_clear_owner THEN
        v_owner := NULL;
    ELSIF nullif(trim(coalesce(p_owner_email, '')), '') IS NOT NULL THEN
        SELECT id INTO v_owner FROM users
         WHERE lower(primary_email) = lower(trim(p_owner_email));
        IF v_owner IS NULL THEN
            -- Refused rather than left unassigned. A reviewer who typed an
            -- address that is not a user needs to know, and silently keeping
            -- the extractor's guess is how a mistyped owner becomes nobody's
            -- work.
            RAISE EXCEPTION 'no user with the email %', p_owner_email;
        END IF;
    END IF;

    IF p_clear_project THEN
        v_project := NULL;
    ELSIF nullif(trim(coalesce(p_project, '')), '') IS NOT NULL THEN
        SELECT id INTO v_project FROM projects WHERE slug = trim(p_project);
        IF v_project IS NULL THEN
            -- Under RLS: a project the reviewer cannot read is one they cannot
            -- assign to, and it fails as "no such project" rather than
            -- confirming it exists. Assigning INTO a project they cannot read
            -- would move a statement somewhere they cannot check it again.
            RAISE EXCEPTION 'no project with the slug %', p_project;
        END IF;
    END IF;

    -- Reclassifying is tightening only. Widening a classification is a
    -- protected action -- knowledge.classification.downgrade is an ungrantable
    -- API scope precisely so it cannot happen through a token -- and the
    -- review surface must not be the back door to it. A reviewer who believes
    -- a restricted statement is really internal has to take that decision
    -- somewhere it is audited as a downgrade.
    IF v_visibility <> v_old_visibility THEN
        IF array_position(ARRAY['internal', 'confidential', 'restricted'], v_visibility)
           < array_position(ARRAY['internal', 'confidential', 'restricted'], v_old_visibility) THEN
            RAISE EXCEPTION
                'a review may tighten a classification but not widen it: % to % needs a downgrade decision',
                v_old_visibility, v_visibility;
        END IF;
    END IF;

    -- A project-scoped statement must stay readable by its own project's
    -- members, so moving it is checked against what the reviewer may read --
    -- the same predicate every other read uses, rather than a second rule.
    IF v_project IS DISTINCT FROM v_cand.proposed_project_id
       AND v_project IS NOT NULL
       AND NOT can_read_project(v_project) THEN
        RAISE EXCEPTION 'the reviewer may not assign a candidate to project %', p_project;
    END IF;

    INSERT INTO knowledge_reviews
        (candidate_id, reviewer_user_id, decision, reason, edited_statement)
    VALUES (p_candidate_id, v_user, p_decision, p_reason, p_edited);

    UPDATE knowledge_candidates
       SET status = v_status,
           statement = coalesce(NULLIF(p_edited, ''), statement),
           candidate_type = v_type,
           visibility = v_visibility,
           proposed_owner_user_id = v_owner,
           proposed_project_id = v_project
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
        'record_id', v_record,
        -- What the row now says, so a caller can show the correction back
        -- rather than re-reading to find out what it did.
        'type', v_cand.candidate_type,
        'visibility', v_cand.visibility,
        -- Whether the review changed them, so a caller can show the
        -- correction back rather than diffing against what it sent.
        'reclassified', v_cand.candidate_type IS DISTINCT FROM v_old_type,
        'reclassified_visibility', v_cand.visibility IS DISTINCT FROM v_old_visibility);
END
$$;

REVOKE ALL ON FUNCTION review_candidate(uuid, text, text, text, text, text, text, text, boolean, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION review_candidate(uuid, text, text, text, text, text, text, text, boolean, boolean) TO workgraph_app;

COMMIT;
