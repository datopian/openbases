-- 0005_marketing.sql — marketing signals and packets.
-- Work package: WP-G1. Plan section 5.
--
-- Marketing consumes approved summaries and evidence. It does not read project
-- source code or unapproved client detail, and the schema is what makes that
-- structural rather than procedural.

BEGIN;

CREATE TABLE marketing_signals (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    work_ref_id   uuid REFERENCES work_refs(id) ON DELETE SET NULL,
    signal_type   text NOT NULL CHECK (signal_type IN
                    ('release', 'benchmark', 'client-outcome', 'insight', 'event', 'oss-milestone')),
    summary       text NOT NULL,
    -- Inherited from the originating project and its sources.
    visibility    text NOT NULL CHECK (visibility IN ('internal', 'confidential', 'restricted')),
    created_by_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX marketing_signals_project ON marketing_signals (project_id, created_at DESC);

CREATE TABLE marketing_packets (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    signal_id     uuid REFERENCES marketing_signals(id) ON DELETE SET NULL,
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    claim         text NOT NULL,
    evidence_refs text[] NOT NULL DEFAULT '{}' CHECK (array_length(evidence_refs, 1) >= 1),
    audience      text NOT NULL,
    -- A technical claim requires a named technical reviewer (plan section 5.3).
    technical_reviewer_user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    technical_reviewed_at timestamptz,
    -- Client information is private by default. Using a client's name, quote,
    -- metric, or case-study detail requires explicit approval.
    client_approved boolean NOT NULL DEFAULT false,
    client_approved_by_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    permitted_names text[] NOT NULL DEFAULT '{}',
    visibility    text NOT NULL CHECK (visibility IN ('internal', 'confidential', 'restricted')),
    embargo_until timestamptz,
    channels      text[] NOT NULL DEFAULT '{}',
    caveats       text,
    status        text NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft', 'approved', 'published', 'retracted')),
    created_at    timestamptz NOT NULL DEFAULT now(),

    -- A packet naming anyone must carry the client approval and the approver.
    CONSTRAINT names_require_client_approval
        CHECK (array_length(permitted_names, 1) IS NULL
               OR (client_approved AND client_approved_by_user_id IS NOT NULL)),
    -- A restricted project's packet cannot be approved for publication without
    -- explicit client approval. This is the marketing side of ADR-0013.
    CONSTRAINT restricted_requires_client_approval
        CHECK (visibility <> 'restricted' OR status = 'draft' OR client_approved),
    CONSTRAINT approved_requires_technical_review
        CHECK (status = 'draft' OR technical_reviewed_at IS NOT NULL),
    CONSTRAINT embargo_respected
        CHECK (status <> 'published' OR embargo_until IS NULL OR embargo_until <= now())
);

CREATE INDEX marketing_packets_project ON marketing_packets (project_id, status);

COMMIT;
