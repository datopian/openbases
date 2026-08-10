-- 0003_evaluation.sql — agent runs, corrections, evaluation, improvement.
-- Work package: WP-H6. Plan section 14.8.
--
-- This schema exists so that "the system improved" is a measurable claim rather
-- than an assertion. Every improvement carries evidence, a replay result, a
-- named approver who is not its author, and a before/after measurement.

BEGIN;

CREATE TABLE agent_runs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_profile_id uuid REFERENCES agent_profiles(id) ON DELETE SET NULL,
    project_id      uuid REFERENCES projects(id) ON DELETE SET NULL,
    work_ref_id     uuid REFERENCES work_refs(id) ON DELETE SET NULL,
    execution_cell_id uuid REFERENCES execution_cells(id) ON DELETE SET NULL,
    objective       text,
    model           text,
    model_version   text,
    prompt_version  text,
    started_at      timestamptz NOT NULL DEFAULT now(),
    ended_at        timestamptz,
    duration_ms     bigint CHECK (duration_ms >= 0),
    retries         integer NOT NULL DEFAULT 0 CHECK (retries >= 0),
    cost_cents      integer CHECK (cost_cents >= 0),
    status          text NOT NULL DEFAULT 'running'
                    CHECK (status IN ('running', 'completed', 'failed', 'stalled', 'terminated')),
    CONSTRAINT ended_after_started CHECK (ended_at IS NULL OR ended_at >= started_at)
);

CREATE INDEX agent_runs_project_time ON agent_runs (project_id, started_at DESC);

CREATE TABLE agent_run_outcomes (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_run_id  uuid NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    tests_passed  boolean,
    review_findings integer CHECK (review_findings >= 0),
    production_outcome text CHECK (production_outcome IN ('shipped', 'reverted', 'abandoned', 'pending')),
    security_events integer NOT NULL DEFAULT 0 CHECK (security_events >= 0),
    policy_events integer NOT NULL DEFAULT 0 CHECK (policy_events >= 0),
    recorded_at   timestamptz NOT NULL DEFAULT now()
);

-- A human correcting an agent is the highest-value signal the pilot produces.
CREATE TABLE human_corrections (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_run_id  uuid REFERENCES agent_runs(id) ON DELETE CASCADE,
    candidate_id  uuid REFERENCES knowledge_candidates(id) ON DELETE CASCADE,
    corrected_by  uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    correction_type text NOT NULL CHECK (correction_type IN
                      ('rejected_output', 'edited_output', 'reclassified', 'reassigned',
                       'changed_scope', 'blocked_action', 'other')),
    before_value  text,
    after_value   text,
    reason        text NOT NULL,
    corrected_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT correction_has_subject
        CHECK (num_nonnulls(agent_run_id, candidate_id) >= 1)
);

CREATE INDEX human_corrections_type_time ON human_corrections (correction_type, corrected_at DESC);

CREATE TABLE evaluation_cases (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug          text NOT NULL UNIQUE,
    suite         text NOT NULL CHECK (suite IN ('golden', 'regression', 'safety')),
    description   text NOT NULL,
    -- Fixtures are synthetic or redacted. A real transcript is never committed,
    -- so a case referencing one points at its source and fetches the encrypted
    -- snapshot with the caller's permissions.
    fixture_path  text,
    source_id     uuid REFERENCES knowledge_sources(id) ON DELETE SET NULL,
    -- Cases derived from a real correction close the loop from failure to test.
    derived_from_correction_id uuid REFERENCES human_corrections(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE evaluation_runs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    case_id       uuid NOT NULL REFERENCES evaluation_cases(id) ON DELETE CASCADE,
    improvement_proposal_id uuid,
    environment   text NOT NULL CHECK (environment IN ('local', 'ci', 'staging', 'production')),
    passed        boolean NOT NULL,
    score         numeric(5,4) CHECK (score >= 0 AND score <= 1),
    details       jsonb NOT NULL DEFAULT '{}'::jsonb,
    ran_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX evaluation_runs_proposal ON evaluation_runs (improvement_proposal_id);

CREATE TABLE prompt_versions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL,
    version     integer NOT NULL CHECK (version > 0),
    git_path    text NOT NULL,
    git_commit  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (name, version)
);

CREATE TABLE formula_versions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL,
    version     integer NOT NULL CHECK (version > 0),
    git_path    text NOT NULL,
    git_commit  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (name, version)
);

CREATE TABLE improvement_proposals (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    work_ref_id   uuid REFERENCES work_refs(id) ON DELETE SET NULL,
    target        text NOT NULL CHECK (target IN
                    ('code', 'prompt', 'formula', 'skill', 'policy', 'test', 'runbook')),
    problem_statement text NOT NULL,
    expected_improvement text NOT NULL,
    rollback_plan text NOT NULL,
    pull_request_url text,
    -- The proposer may be an agent; the approver may not be the proposer.
    proposed_by_agent_id uuid REFERENCES agent_profiles(id) ON DELETE SET NULL,
    proposed_by_user_id  uuid REFERENCES users(id) ON DELETE SET NULL,
    approved_by_user_id  uuid REFERENCES users(id) ON DELETE RESTRICT,
    approved_at   timestamptz,
    status        text NOT NULL DEFAULT 'proposed'
                  CHECK (status IN ('proposed', 'evaluating', 'approved', 'rejected', 'deployed', 'rolled_back')),
    -- Before/after measurement. An improvement with no measurement is a change.
    baseline_metrics jsonb,
    result_metrics   jsonb,
    deployed_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT proposal_has_proposer
        CHECK (num_nonnulls(proposed_by_agent_id, proposed_by_user_id) = 1),
    -- Nothing approves itself (ADR-0014). A proposal authored by a human cannot
    -- be approved by that same human.
    CONSTRAINT no_self_approval
        CHECK (approved_by_user_id IS NULL
               OR proposed_by_user_id IS NULL
               OR approved_by_user_id <> proposed_by_user_id),
    CONSTRAINT approved_names_approver
        CHECK (status NOT IN ('approved', 'deployed') OR approved_by_user_id IS NOT NULL),
    CONSTRAINT deployed_has_measurement
        CHECK (status <> 'deployed' OR baseline_metrics IS NOT NULL)
);

ALTER TABLE evaluation_runs
    ADD CONSTRAINT evaluation_runs_proposal_fk
    FOREIGN KEY (improvement_proposal_id) REFERENCES improvement_proposals(id) ON DELETE CASCADE;

COMMIT;
