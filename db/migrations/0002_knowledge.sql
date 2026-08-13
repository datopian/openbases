-- 0002_knowledge.sql — sources, evidence, candidates, review, records, context.
-- Work packages: WP-H2, WP-H3, WP-H4, WP-H5. Plan sections 14.3 to 14.7.
--
-- The governing rule of this schema: a candidate is not memory, and a record
-- cannot exist without provenance. Both are enforced by constraints, not by
-- application discipline.

BEGIN;

-- ---------------------------------------------------------------------------
-- Sources and evidence
-- ---------------------------------------------------------------------------

-- A registered source. The provider stays canonical for raw human-authored
-- content; this table records identity, revision, ownership and classification
-- so that lineage survives later edits at the provider (plan section 14.3).
CREATE TABLE knowledge_sources (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id   uuid NOT NULL REFERENCES organisations(id) ON DELETE RESTRICT,
    provider          text NOT NULL CHECK (provider IN ('google-meet', 'google-drive', 'github', 'workgraph')),
    provider_source_id text NOT NULL,
    provider_revision text,
    source_type       text NOT NULL CHECK (source_type IN
                        ('transcript', 'smart-note', 'document', 'pull-request', 'deployment', 'metric')),
    owner_user_id     uuid REFERENCES users(id) ON DELETE SET NULL,
    project_id        uuid REFERENCES projects(id) ON DELETE SET NULL,
    captured_at       timestamptz,
    visibility        text NOT NULL CHECK (visibility IN ('internal', 'confidential', 'restricted')),
    retention_class   text NOT NULL DEFAULT 'standard'
                      CHECK (retention_class IN ('standard', 'client-contract', 'legal-hold')),
    -- Set when the provider reports the source deleted. The row and its
    -- snapshots survive: provider deletion does not erase audit evidence
    -- unless retention policy allows it (plan section 15.3).
    deleted_at_provider timestamptz,
    -- Raised when a source contains instruction-shaped text. Derived candidates
    -- then require two reviewers (policies/knowledge-classification.yaml).
    prompt_injection_suspected boolean NOT NULL DEFAULT false,
    registered_by     uuid REFERENCES users(id) ON DELETE SET NULL,
    registered_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_source_id, provider_revision)
);

CREATE INDEX knowledge_sources_project ON knowledge_sources (project_id);

-- Immutable encrypted evidence. The checksum is of the encrypted object in R2.
CREATE TABLE source_snapshots (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id     uuid NOT NULL REFERENCES knowledge_sources(id) ON DELETE RESTRICT,
    r2_key        text NOT NULL UNIQUE,
    checksum_sha256 text NOT NULL CHECK (checksum_sha256 ~ '^[0-9a-f]{64}$'),
    byte_size     bigint NOT NULL CHECK (byte_size >= 0),
    encrypted     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    verified_at   timestamptz,
    -- An unencrypted snapshot is never acceptable: these hold client material.
    CONSTRAINT snapshots_are_encrypted CHECK (encrypted)
);

-- The source ACL as it stood at capture time, so a later permission change is
-- detectable rather than invisible.
CREATE TABLE source_acl_entries (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id    uuid NOT NULL REFERENCES knowledge_sources(id) ON DELETE CASCADE,
    principal    text NOT NULL,
    principal_type text NOT NULL CHECK (principal_type IN ('user', 'group', 'domain', 'anyone')),
    role         text NOT NULL,
    observed_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX source_acl_source ON source_acl_entries (source_id);

-- ---------------------------------------------------------------------------
-- Candidates and review
-- ---------------------------------------------------------------------------

-- A machine-extracted statement awaiting human review. Never durable memory.
CREATE TABLE knowledge_candidates (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id      uuid NOT NULL REFERENCES knowledge_sources(id) ON DELETE CASCADE,
    candidate_type text NOT NULL CHECK (candidate_type IN
                     ('task', 'commitment', 'decision', 'risk', 'fact', 'constraint',
                      'lesson', 'preference', 'assumption', 'market-signal', 'question')),
    statement      text NOT NULL CHECK (length(trim(statement)) > 0),
    proposed_owner_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    proposed_project_id    uuid REFERENCES projects(id) ON DELETE SET NULL,
    due_date       date,
    confidence     numeric(4,3) NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    -- Inherited from the source. It is never chosen by the extractor.
    visibility     text NOT NULL CHECK (visibility IN ('internal', 'confidential', 'restricted')),
    -- Where in the source this came from. A candidate with no span cannot be
    -- checked by a reviewer, so it is required.
    source_spans   jsonb NOT NULL CHECK (jsonb_array_length(source_spans) > 0),
    was_inferred   boolean NOT NULL DEFAULT true,
    extractor_prompt_version text,
    status         text NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'accepted', 'edited_accepted', 'rejected', 'deferred', 'merged')),
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX knowledge_candidates_pending ON knowledge_candidates (status, created_at)
    WHERE status = 'pending';

-- Every review decision, including rejections. Rejections and edits are the
-- evaluation data that drives the improvement loop (plan section 14.8).
CREATE TABLE knowledge_reviews (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    candidate_id  uuid NOT NULL REFERENCES knowledge_candidates(id) ON DELETE CASCADE,
    -- A reviewer is always a named human. Agent profiles cannot review, which
    -- is why this column references users and nothing else.
    reviewer_user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    decision      text NOT NULL CHECK (decision IN
                    ('accept', 'edit_and_accept', 'reject', 'defer', 'merge')),
    reason        text,
    edited_statement text,
    decided_at    timestamptz NOT NULL DEFAULT now(),
    -- An edit must say what it changed to, or the correction is not recoverable
    -- as evaluation data.
    CONSTRAINT edit_requires_statement
        CHECK (decision <> 'edit_and_accept' OR edited_statement IS NOT NULL),
    CONSTRAINT reject_requires_reason
        CHECK (decision <> 'reject' OR reason IS NOT NULL)
);

CREATE INDEX knowledge_reviews_candidate ON knowledge_reviews (candidate_id);

-- ---------------------------------------------------------------------------
-- Durable records
-- ---------------------------------------------------------------------------

CREATE TABLE knowledge_records (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id uuid NOT NULL REFERENCES organisations(id) ON DELETE RESTRICT,
    record_type   text NOT NULL CHECK (record_type IN
                    ('decision', 'constraint', 'fact', 'lesson', 'preference', 'assumption')),
    statement     text NOT NULL CHECK (length(trim(statement)) > 0),
    scope         text NOT NULL CHECK (scope IN ('company', 'function', 'project', 'personal')),
    project_id    uuid REFERENCES projects(id) ON DELETE SET NULL,
    function_id   uuid REFERENCES functions(id) ON DELETE SET NULL,
    owner_user_id uuid REFERENCES users(id) ON DELETE RESTRICT,
    -- Nothing becomes a record without a named human accepting it.
    reviewer_user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    reviewed_at   timestamptz NOT NULL DEFAULT now(),
    visibility    text NOT NULL CHECK (visibility IN ('internal', 'confidential', 'restricted')),
    confidence    numeric(4,3) CHECK (confidence >= 0 AND confidence <= 1),
    valid_from    date NOT NULL DEFAULT CURRENT_DATE,
    review_after  date,
    supersedes_id uuid REFERENCES knowledge_records(id) ON DELETE SET NULL,
    superseded_by_id uuid REFERENCES knowledge_records(id) ON DELETE SET NULL,
    status        text NOT NULL DEFAULT 'accepted'
                  CHECK (status IN ('proposed', 'accepted', 'disputed', 'superseded', 'expired')),
    -- Set when a human authored the statement directly rather than accepting an
    -- extraction. Provenance is then the named author.
    human_authored boolean NOT NULL DEFAULT false,
    author_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    -- Markdown location in company-workgraph, when the record is long-form.
    git_path      text,
    created_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT project_scope_has_project
        CHECK (scope <> 'project' OR project_id IS NOT NULL),
    CONSTRAINT function_scope_has_function
        CHECK (scope <> 'function' OR function_id IS NOT NULL),
    -- Supersede, never overwrite. A retired record names its successor.
    CONSTRAINT superseded_names_successor
        CHECK (status <> 'superseded' OR superseded_by_id IS NOT NULL),
    -- Time-sensitive types must declare when they go stale, or a stale fact
    -- silently keeps being served as current (plan section 14.7).
    CONSTRAINT time_sensitive_needs_review_date
        CHECK (record_type NOT IN ('fact', 'constraint', 'assumption') OR review_after IS NOT NULL),
    CONSTRAINT human_authored_names_author
        CHECK (NOT human_authored OR author_user_id IS NOT NULL),
    CONSTRAINT no_self_supersession
        CHECK (supersedes_id IS DISTINCT FROM id AND superseded_by_id IS DISTINCT FROM id)
);

CREATE INDEX knowledge_records_scope ON knowledge_records (scope, status);
CREATE INDEX knowledge_records_stale ON knowledge_records (review_after)
    WHERE status = 'accepted' AND review_after IS NOT NULL;

-- The evidence chain. A record with no source must be human-authored; the
-- trigger below enforces that, because a CHECK cannot see another table.
CREATE TABLE knowledge_record_sources (
    record_id  uuid NOT NULL REFERENCES knowledge_records(id) ON DELETE CASCADE,
    source_id  uuid NOT NULL REFERENCES knowledge_sources(id) ON DELETE RESTRICT,
    candidate_id uuid REFERENCES knowledge_candidates(id) ON DELETE SET NULL,
    PRIMARY KEY (record_id, source_id)
);

CREATE OR REPLACE FUNCTION knowledge_record_requires_provenance() RETURNS trigger AS $$
BEGIN
    IF NEW.human_authored THEN
        RETURN NEW;   -- provenance is the named author
    END IF;
    IF NOT EXISTS (SELECT 1 FROM knowledge_record_sources WHERE record_id = NEW.id) THEN
        RAISE EXCEPTION
            'knowledge record % has no source and is not human_authored', NEW.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Deferred so that a record and its sources can be inserted in one transaction.
CREATE CONSTRAINT TRIGGER knowledge_record_provenance
    AFTER INSERT OR UPDATE ON knowledge_records
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION knowledge_record_requires_provenance();

-- Contradictions and relationships between records. A contradiction creates a
-- review item; it is never resolved automatically (plan section 14.7).
CREATE TABLE knowledge_relations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    from_record_id uuid NOT NULL REFERENCES knowledge_records(id) ON DELETE CASCADE,
    to_record_id   uuid NOT NULL REFERENCES knowledge_records(id) ON DELETE CASCADE,
    relation    text NOT NULL CHECK (relation IN ('contradicts', 'refines', 'relates', 'derived_from')),
    detected_at timestamptz NOT NULL DEFAULT now(),
    resolved_by_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    resolved_at timestamptz,
    UNIQUE (from_record_id, to_record_id, relation),
    CONSTRAINT no_self_relation CHECK (from_record_id <> to_record_id)
);

-- ---------------------------------------------------------------------------
-- Context packs
-- ---------------------------------------------------------------------------

-- A permission-filtered bundle assembled for one objective. A cached
-- projection: always regenerable, never a source of truth.
CREATE TABLE context_packs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    requested_for_user_id  uuid REFERENCES users(id) ON DELETE CASCADE,
    requested_for_agent_id uuid REFERENCES agent_profiles(id) ON DELETE CASCADE,
    work_ref_id   uuid REFERENCES work_refs(id) ON DELETE SET NULL,
    project_id    uuid REFERENCES projects(id) ON DELETE CASCADE,
    -- The highest classification of anything included. The pack inherits it.
    visibility    text NOT NULL CHECK (visibility IN ('internal', 'confidential', 'restricted')),
    token_budget  integer CHECK (token_budget > 0),
    generated_at  timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz,
    -- A pack is built for a principal. One of the two must be set, so a pack
    -- can never exist without an identity whose permissions filtered it.
    CONSTRAINT pack_has_principal
        CHECK (num_nonnulls(requested_for_user_id, requested_for_agent_id) = 1)
);

CREATE TABLE context_pack_items (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    context_pack_id uuid NOT NULL REFERENCES context_packs(id) ON DELETE CASCADE,
    record_id      uuid REFERENCES knowledge_records(id) ON DELETE CASCADE,
    work_ref_id    uuid REFERENCES work_refs(id) ON DELETE CASCADE,
    -- Why this item is in the pack, shown to the reader. Retrieval that cannot
    -- explain itself is not reviewable.
    inclusion_reason text NOT NULL,
    freshness_days integer,
    position       integer NOT NULL CHECK (position >= 0),
    CONSTRAINT item_references_something
        CHECK (num_nonnulls(record_id, work_ref_id) = 1)
);

CREATE INDEX context_pack_items_pack ON context_pack_items (context_pack_id, position);

COMMIT;
