-- 0001_core.sql — identity, registry, work references, events, audit.
-- Work package: WP-C1. Plan sections 12.1, 12.2, 8.2, 7.3.
--
-- UUIDs are used internally; external provider identifiers are preserved in
-- dedicated columns so that a provider rename never breaks a reference.

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- Organisation and identity
-- ---------------------------------------------------------------------------

CREATE TABLE organisations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        text NOT NULL UNIQUE,
    name        text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id uuid NOT NULL REFERENCES organisations(id) ON DELETE RESTRICT,
    display_name    text NOT NULL,
    -- Email is an attribute, not the key: it changes and can be reassigned.
    primary_email   text,
    status          text NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'suspended', 'removed')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- One user may have several provider identities. The immutable provider
-- subject is the join key, never the email address (plan section 8.1).
CREATE TABLE identities (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider    text NOT NULL CHECK (provider IN ('cloudflare_access', 'google', 'github')),
    subject     text NOT NULL,
    email       text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, subject)
);

CREATE TABLE roles (
    name        text PRIMARY KEY,
    description text NOT NULL
);

INSERT INTO roles (name, description) VALUES
    ('organisation_admin', 'Identities, policy, integrations, all projects'),
    ('executive',          'Portfolio visibility, approvals, sensitive summaries'),
    ('portfolio_lead',     'Projects within a portfolio'),
    ('function_lead',      'Function work and campaigns'),
    ('project_lead',       'Full project context and execution within policy'),
    ('backup_operator',    'Continuity, review, and takeover rights'),
    ('contributor',        'Assigned work and permitted project context'),
    ('observer',           'Read-only project status'),
    ('external_client',    'Explicitly scoped, read-only or approval-only');

-- ---------------------------------------------------------------------------
-- Registry
-- ---------------------------------------------------------------------------

CREATE TABLE portfolios (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id uuid NOT NULL REFERENCES organisations(id) ON DELETE RESTRICT,
    slug            text NOT NULL,
    name            text NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('client', 'product', 'oss', 'internal')),
    UNIQUE (organisation_id, slug)
);

CREATE TABLE functions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id uuid NOT NULL REFERENCES organisations(id) ON DELETE RESTRICT,
    slug            text NOT NULL,
    name            text NOT NULL,
    lead_user_id    uuid REFERENCES users(id) ON DELETE SET NULL,
    UNIQUE (organisation_id, slug)
);

CREATE TABLE execution_nodes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hostname    text NOT NULL UNIQUE,
    environment text NOT NULL CHECK (environment IN ('staging', 'production')),
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- An execution cell is the security and runtime boundary. Cells are drawn by
-- trust domain, not by employee (plan section 7.4).
CREATE TABLE execution_cells (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    execution_node_id   uuid NOT NULL REFERENCES execution_nodes(id) ON DELETE RESTRICT,
    slug                text NOT NULL UNIQUE,
    system_username     text NOT NULL UNIQUE,
    trust_domain        text NOT NULL,
    max_concurrent_agents integer NOT NULL DEFAULT 2 CHECK (max_concurrent_agents > 0),
    cpu_quota_percent   integer CHECK (cpu_quota_percent > 0),
    memory_limit_mb     integer CHECK (memory_limit_mb > 0),
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE projects (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id   uuid NOT NULL REFERENCES organisations(id) ON DELETE RESTRICT,
    portfolio_id      uuid REFERENCES portfolios(id) ON DELETE SET NULL,
    function_id       uuid REFERENCES functions(id) ON DELETE SET NULL,
    slug              text NOT NULL,
    name              text NOT NULL,
    objective         text,
    visibility        text NOT NULL DEFAULT 'internal'
                      CHECK (visibility IN ('internal', 'confidential', 'restricted')),
    primary_owner_id  uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    -- A backup operator is mandatory: full-cycle ownership must not become a
    -- single point of failure (plan section 2.1).
    backup_owner_id   uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    execution_cell_id uuid REFERENCES execution_cells(id) ON DELETE RESTRICT,
    status            text NOT NULL DEFAULT 'active'
                      CHECK (status IN ('active', 'paused', 'closed')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organisation_id, slug),
    CONSTRAINT backup_owner_differs CHECK (backup_owner_id <> primary_owner_id),
    -- A restricted project always has its own cell (plan section 7.4).
    CONSTRAINT restricted_requires_cell
        CHECK (visibility <> 'restricted' OR execution_cell_id IS NOT NULL)
);

CREATE TABLE project_memberships (
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role_name   text NOT NULL REFERENCES roles(name),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, user_id, role_name)
);

-- Scoped role grants. A NULL project_id is an organisation-scoped grant.
CREATE TABLE role_grants (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role_name       text NOT NULL REFERENCES roles(name),
    organisation_id uuid NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    project_id      uuid REFERENCES projects(id) ON DELETE CASCADE,
    granted_by      uuid REFERENCES users(id) ON DELETE SET NULL,
    granted_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz
);

CREATE UNIQUE INDEX role_grants_unique
    ON role_grants (user_id, role_name, organisation_id, COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid));

CREATE TABLE project_repositories (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    provider    text NOT NULL DEFAULT 'github' CHECK (provider IN ('github')),
    owner       text NOT NULL,
    name        text NOT NULL,
    -- Preserve the provider's stable numeric ID so a repository rename does
    -- not orphan the link.
    provider_id text,
    default_branch text NOT NULL DEFAULT 'main',
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, owner, name)
);

-- Agent profiles are service actors with declared purpose and scope, not users
-- with unlimited access (plan section 8.3).
CREATE TABLE agent_profiles (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id   uuid NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    slug              text NOT NULL,
    kind              text NOT NULL CHECK (kind IN
                        ('personal_chief_of_staff', 'project_coordinator',
                         'function_coordinator', 'worker', 'reviewer', 'release')),
    owner_user_id     uuid REFERENCES users(id) ON DELETE SET NULL,
    project_id        uuid REFERENCES projects(id) ON DELETE CASCADE,
    allowed_actions   text[] NOT NULL DEFAULT '{}',
    max_budget_cents  integer CHECK (max_budget_cents >= 0),
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organisation_id, slug)
);

-- ---------------------------------------------------------------------------
-- Work graph references
-- ---------------------------------------------------------------------------

CREATE TABLE beads_databases (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id   uuid NOT NULL REFERENCES organisations(id) ON DELETE RESTRICT,
    execution_cell_id uuid REFERENCES execution_cells(id) ON DELETE RESTRICT,
    project_id        uuid REFERENCES projects(id) ON DELETE CASCADE,
    owner_user_id     uuid REFERENCES users(id) ON DELETE CASCADE,
    name              text NOT NULL,
    scope             text NOT NULL CHECK (scope IN ('company', 'project', 'function', 'personal')),
    dolt_port         integer CHECK (dolt_port BETWEEN 1024 AND 65535),
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organisation_id, name),
    -- A personal graph belongs to exactly one person and never to a project.
    CONSTRAINT personal_scope_has_owner
        CHECK (scope <> 'personal' OR (owner_user_id IS NOT NULL AND project_id IS NULL))
);

-- The stable identity of a work item outside Beads. The full tuple is stored
-- because project prefixes can collide and work can migrate between cells
-- (plan section 7.3).
CREATE TABLE work_refs (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id   uuid NOT NULL REFERENCES organisations(id) ON DELETE RESTRICT,
    execution_cell_id uuid REFERENCES execution_cells(id) ON DELETE RESTRICT,
    beads_database_id uuid NOT NULL REFERENCES beads_databases(id) ON DELETE CASCADE,
    bead_id           text NOT NULL,
    -- Cached projection of Beads state. Beads remains canonical; these columns
    -- exist so the UI can rank and filter without querying every cell.
    title             text,
    kind              text,
    status            text,
    visibility        text CHECK (visibility IN ('internal', 'confidential', 'restricted')),
    project_id        uuid REFERENCES projects(id) ON DELETE SET NULL,
    last_seen_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organisation_id, execution_cell_id, beads_database_id, bead_id)
);

-- Relationships that cross Beads databases. Beads cannot express these, so the
-- control plane owns them as durable, audited edges (plan section 2.3).
CREATE TABLE work_links (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    from_work_ref uuid NOT NULL REFERENCES work_refs(id) ON DELETE CASCADE,
    to_work_ref   uuid NOT NULL REFERENCES work_refs(id) ON DELETE CASCADE,
    relation      text NOT NULL CHECK (relation IN
                    ('implements', 'blocks', 'informs', 'relates', 'supersedes', 'evidences')),
    created_by    uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (from_work_ref, to_work_ref, relation),
    CONSTRAINT no_self_link CHECK (from_work_ref <> to_work_ref)
);

-- ---------------------------------------------------------------------------
-- Event spine and audit
-- ---------------------------------------------------------------------------

-- Append-only domain events. Every event carries provenance, an idempotency
-- key, and a correlation ID (plan section 12.2).
CREATE TABLE events (
    id              bigserial PRIMARY KEY,
    occurred_at     timestamptz NOT NULL DEFAULT now(),
    type            text NOT NULL,
    schema_version  integer NOT NULL DEFAULT 1,
    source_system   text NOT NULL,
    source_id       text,
    actor_user_id   uuid REFERENCES users(id) ON DELETE SET NULL,
    actor_agent_id  uuid REFERENCES agent_profiles(id) ON DELETE SET NULL,
    project_id      uuid REFERENCES projects(id) ON DELETE SET NULL,
    visibility      text NOT NULL DEFAULT 'internal'
                    CHECK (visibility IN ('internal', 'confidential', 'restricted')),
    idempotency_key text NOT NULL,
    correlation_id  uuid,
    payload         jsonb NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (source_system, idempotency_key)
);

CREATE INDEX events_type_time ON events (type, occurred_at DESC);
CREATE INDEX events_project_time ON events (project_id, occurred_at DESC);

-- Raw webhook receipts, stored before processing so that a delivery can be
-- replayed and duplicates rejected (plan section 9.3).
CREATE TABLE github_deliveries (
    delivery_id   text PRIMARY KEY,
    event_type    text NOT NULL,
    received_at   timestamptz NOT NULL DEFAULT now(),
    processed_at  timestamptz,
    payload       jsonb NOT NULL
);

-- Immutable audit records. Append-only: there is no UPDATE or DELETE path in
-- the application, and the export to retention-locked storage is the archive.
CREATE TABLE audit_log (
    id              bigserial PRIMARY KEY,
    at              timestamptz NOT NULL DEFAULT now(),
    actor_user_id   uuid REFERENCES users(id) ON DELETE SET NULL,
    actor_agent_id  uuid REFERENCES agent_profiles(id) ON DELETE SET NULL,
    action          text NOT NULL,
    target_type     text NOT NULL,
    target_id       text,
    project_id      uuid REFERENCES projects(id) ON DELETE SET NULL,
    outcome         text NOT NULL CHECK (outcome IN ('allowed', 'denied', 'executed', 'failed')),
    reason          text,
    correlation_id  uuid,
    evidence_refs   text[] NOT NULL DEFAULT '{}'
);

CREATE INDEX audit_log_actor_time ON audit_log (actor_user_id, at DESC);
CREATE INDEX audit_log_project_time ON audit_log (project_id, at DESC);

-- Refuse mutation of the audit trail at the database level, so that an
-- application bug or a compromised service account cannot rewrite history.
CREATE OR REPLACE FUNCTION audit_log_is_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_log_no_update
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_is_append_only();

CREATE TRIGGER events_no_update
    BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION audit_log_is_append_only();

COMMIT;
