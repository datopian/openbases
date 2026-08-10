-- 0006_operations.sql — usage, budgets, credential references, policy bundles.
-- Work packages: WP-B3, WP-I1. Plan sections 10.1, 11.4, 15.2.

BEGIN;

CREATE TABLE usage_records (
    id            bigserial PRIMARY KEY,
    project_id    uuid REFERENCES projects(id) ON DELETE CASCADE,
    agent_run_id  uuid REFERENCES agent_runs(id) ON DELETE SET NULL,
    provider      text NOT NULL,
    model         text,
    input_tokens  bigint CHECK (input_tokens >= 0),
    output_tokens bigint CHECK (output_tokens >= 0),
    cost_cents    integer NOT NULL DEFAULT 0 CHECK (cost_cents >= 0),
    occurred_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX usage_records_project_time ON usage_records (project_id, occurred_at DESC);

CREATE TABLE budget_limits (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid REFERENCES projects(id) ON DELETE CASCADE,
    execution_cell_id uuid REFERENCES execution_cells(id) ON DELETE CASCADE,
    daily_cost_cents  integer NOT NULL CHECK (daily_cost_cents >= 0),
    max_concurrent_agents integer NOT NULL CHECK (max_concurrent_agents > 0),
    max_runtime_minutes   integer CHECK (max_runtime_minutes > 0),
    set_by_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT budget_has_subject
        CHECK (num_nonnulls(project_id, execution_cell_id) = 1)
);

-- References to credentials, never the values. A secret in this table would
-- defeat the entire secret-handling model (plan section 11.4).
CREATE TABLE credential_refs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL UNIQUE,
    credential_class text NOT NULL CHECK (credential_class IN
                       ('github_app_key', 'github_webhook_secret', 'agent_provider',
                        'cloudflare_api_token', 'hetzner_api_token', 'google_service_account',
                        'database', 'age_key', 'other')),
    -- Where the value lives: a SOPS path, a systemd credential name, or a
    -- Cloudflare secret binding. Never the value itself.
    storage_location text NOT NULL,
    owner_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    execution_cell_id uuid REFERENCES execution_cells(id) ON DELETE SET NULL,
    expires_at    timestamptz,
    last_rotated_at timestamptz,
    rotation_period_days integer CHECK (rotation_period_days > 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- A storage location that looks like a secret value is a mistake worth
    -- catching at write time.
    CONSTRAINT storage_location_is_a_reference
        CHECK (storage_location !~ '^(gh[pous]_|github_pat_|sk-|AKIA|AIza|-----BEGIN)')
);

CREATE TABLE policy_bundles (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL,
    version       integer NOT NULL CHECK (version > 0),
    git_path      text NOT NULL,
    git_commit    text NOT NULL,
    -- Parsed content, so policy evaluation does not read the filesystem on the
    -- hot path. The Git file remains canonical.
    content       jsonb NOT NULL,
    active        boolean NOT NULL DEFAULT false,
    activated_by_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    activated_at  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (name, version),
    CONSTRAINT active_bundle_names_activator
        CHECK (NOT active OR activated_by_user_id IS NOT NULL)
);

-- Only one version of a bundle may be active at a time; two active versions
-- would make policy evaluation nondeterministic.
CREATE UNIQUE INDEX policy_bundles_one_active ON policy_bundles (name) WHERE active;

CREATE TABLE google_subscriptions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subscription_name text NOT NULL UNIQUE,
    target_resource   text NOT NULL,
    event_types   text[] NOT NULL CHECK (array_length(event_types, 1) >= 1),
    -- Subscriptions expire. Tracking expiry is what prevents silent ingestion
    -- loss (plan section 14.2).
    expires_at    timestamptz NOT NULL,
    last_renewed_at timestamptz,
    last_reconciled_at timestamptz,
    state         text NOT NULL DEFAULT 'active'
                  CHECK (state IN ('active', 'expiring', 'expired', 'deleted', 'error')),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX google_subscriptions_expiry ON google_subscriptions (expires_at)
    WHERE state IN ('active', 'expiring');

CREATE TABLE google_deliveries (
    message_id    text PRIMARY KEY,
    subscription_id uuid REFERENCES google_subscriptions(id) ON DELETE SET NULL,
    event_type    text NOT NULL,
    received_at   timestamptz NOT NULL DEFAULT now(),
    processed_at  timestamptz,
    payload       jsonb NOT NULL
);

COMMIT;
