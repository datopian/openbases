-- 0004_attention.sql — attention items, approvals, narratives.
-- Work packages: WP-F1, WP-F2. Plan sections 10.2, 13.
--
-- The load-bearing rule here is approval digest binding: an approval authorises
-- one exact artefact. If the diff, plan, or image digest changes, the approval
-- is void.

BEGIN;

CREATE TABLE approval_requests (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    action_type     text NOT NULL,
    target_type     text NOT NULL,
    target_id       text,
    project_id      uuid REFERENCES projects(id) ON DELETE CASCADE,
    work_ref_id     uuid REFERENCES work_refs(id) ON DELETE SET NULL,
    -- The digest of the exact proposed action: diff, plan artefact, image
    -- digest, or canonicalised parameters. This is what is approved.
    action_digest   text NOT NULL CHECK (action_digest ~ '^[0-9a-f]{64}$'),
    proposed_parameters jsonb NOT NULL DEFAULT '{}'::jsonb,
    risk_level      text NOT NULL CHECK (risk_level IN ('low', 'medium', 'high', 'critical')),
    required_roles  text[] NOT NULL CHECK (array_length(required_roles, 1) >= 1),
    required_approvals integer NOT NULL DEFAULT 1 CHECK (required_approvals >= 1),
    evidence_refs   text[] NOT NULL DEFAULT '{}',
    requested_by_user_id  uuid REFERENCES users(id) ON DELETE SET NULL,
    requested_by_agent_id uuid REFERENCES agent_profiles(id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    status          text NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'approved', 'rejected', 'expired', 'invalidated', 'executed')),
    executed_at     timestamptz,
    execution_result text,

    CONSTRAINT request_has_requester
        CHECK (num_nonnulls(requested_by_user_id, requested_by_agent_id) = 1),
    CONSTRAINT expiry_after_creation CHECK (expires_at > created_at),
    -- High-risk classes need two humans (plan section 14.8, policies/default.yaml).
    CONSTRAINT critical_needs_two CHECK (risk_level <> 'critical' OR required_approvals >= 2)
);

CREATE INDEX approval_requests_pending ON approval_requests (status, expires_at)
    WHERE status = 'pending';

CREATE TABLE approval_decisions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id    uuid NOT NULL REFERENCES approval_requests(id) ON DELETE CASCADE,
    -- Only a human decides. There is no agent column here by design.
    decided_by_user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    decision      text NOT NULL CHECK (decision IN ('approve', 'reject')),
    reason        text,
    -- Recorded again at decision time so that a later digest change is provable
    -- rather than inferred.
    decided_digest text NOT NULL CHECK (decided_digest ~ '^[0-9a-f]{64}$'),
    decided_at    timestamptz NOT NULL DEFAULT now(),
    -- One decision per approver per request: no double-counting toward the
    -- required approval count.
    UNIQUE (request_id, decided_by_user_id),
    CONSTRAINT reject_requires_reason CHECK (decision <> 'reject' OR reason IS NOT NULL)
);

-- An approver may not approve their own request.
CREATE OR REPLACE FUNCTION approval_forbids_self_approval() RETURNS trigger AS $$
DECLARE
    requester uuid;
BEGIN
    SELECT requested_by_user_id INTO requester
    FROM approval_requests WHERE id = NEW.request_id;

    IF requester IS NOT NULL AND requester = NEW.decided_by_user_id THEN
        RAISE EXCEPTION 'user % cannot approve their own request %',
            NEW.decided_by_user_id, NEW.request_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER approval_no_self_approval
    BEFORE INSERT ON approval_decisions
    FOR EACH ROW EXECUTE FUNCTION approval_forbids_self_approval();

-- Decisions are a durable record, not a mutable status field.
CREATE OR REPLACE FUNCTION approval_decisions_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'approval_decisions is append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER approval_decisions_no_update
    BEFORE UPDATE OR DELETE ON approval_decisions
    FOR EACH ROW EXECUTE FUNCTION approval_decisions_append_only();

CREATE TABLE attention_items (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id    uuid REFERENCES projects(id) ON DELETE CASCADE,
    work_ref_id   uuid REFERENCES work_refs(id) ON DELETE CASCADE,
    approval_request_id uuid REFERENCES approval_requests(id) ON DELETE CASCADE,
    record_id     uuid REFERENCES knowledge_records(id) ON DELETE CASCADE,
    rule          text NOT NULL,
    score         numeric(8,4) NOT NULL,
    -- Both the score and why it was computed. An unexplained ranking cannot be
    -- tuned or trusted (plan section 13.1).
    explanation   jsonb NOT NULL,
    visibility    text NOT NULL CHECK (visibility IN ('internal', 'confidential', 'restricted')),
    status        text NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open', 'snoozed', 'delegated', 'resolved')),
    snoozed_until timestamptz,
    delegated_to_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    resolved_at   timestamptz,
    CONSTRAINT snoozed_has_deadline CHECK (status <> 'snoozed' OR snoozed_until IS NOT NULL),
    CONSTRAINT delegated_has_target CHECK (status <> 'delegated' OR delegated_to_user_id IS NOT NULL)
);

CREATE INDEX attention_items_user_open ON attention_items (user_id, score DESC)
    WHERE status = 'open';

CREATE TABLE attention_subscriptions (
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    rule       text NOT NULL,
    enabled    boolean NOT NULL DEFAULT true,
    PRIMARY KEY (user_id, rule)
);

CREATE TABLE narrative_snapshots (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid REFERENCES projects(id) ON DELETE CASCADE,
    scope         text NOT NULL CHECK (scope IN ('project', 'portfolio', 'company')),
    period_start  timestamptz NOT NULL,
    period_end    timestamptz NOT NULL,
    body          text NOT NULL,
    -- Every bullet cites its evidence; a narrative is never canonical.
    evidence_refs jsonb NOT NULL DEFAULT '[]'::jsonb,
    model         text,
    model_version text,
    visibility    text NOT NULL CHECK (visibility IN ('internal', 'confidential', 'restricted')),
    generated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT period_ordered CHECK (period_end >= period_start)
);

CREATE INDEX narrative_snapshots_project_time ON narrative_snapshots (project_id, generated_at DESC);

COMMIT;
