-- 0007_projections.sql — rebuildable retrieval projection.
-- Work package: WP-H5. Plan sections 6.2, 14.6.
--
-- Everything here is disposable. It can be dropped and rebuilt from Beads,
-- GitHub, and the knowledge tables. It must never become an alternative task
-- tracker or an unreviewed company memory.

BEGIN;

CREATE EXTENSION IF NOT EXISTS vector;

-- Embeddings of reviewed knowledge records only. Candidates are never embedded:
-- an unreviewed statement must not become retrievable (ADR-0011).
CREATE TABLE record_embeddings (
    record_id   uuid PRIMARY KEY REFERENCES knowledge_records(id) ON DELETE CASCADE,
    -- The classification is denormalised onto the embedding so that the
    -- permission filter runs in the same query as the similarity search, not
    -- as a post-filter over a ranked list (ADR-0013).
    visibility  text NOT NULL CHECK (visibility IN ('internal', 'confidential', 'restricted')),
    project_id  uuid REFERENCES projects(id) ON DELETE CASCADE,
    embedding   vector(1536) NOT NULL,
    model       text NOT NULL,
    generated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX record_embeddings_visibility ON record_embeddings (visibility, project_id);
CREATE INDEX record_embeddings_vector ON record_embeddings
    USING hnsw (embedding vector_cosine_ops);

-- Keyword search over the same reviewed records, with the same filters.
CREATE TABLE record_search (
    record_id   uuid PRIMARY KEY REFERENCES knowledge_records(id) ON DELETE CASCADE,
    visibility  text NOT NULL CHECK (visibility IN ('internal', 'confidential', 'restricted')),
    project_id  uuid REFERENCES projects(id) ON DELETE CASCADE,
    document    tsvector NOT NULL
);

CREATE INDEX record_search_document ON record_search USING gin (document);
CREATE INDEX record_search_visibility ON record_search (visibility, project_id);

-- Projected pull request state from GitHub, for the project view.
CREATE TABLE pull_request_projections (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id   uuid NOT NULL REFERENCES project_repositories(id) ON DELETE CASCADE,
    number          integer NOT NULL CHECK (number > 0),
    title           text NOT NULL,
    state           text NOT NULL CHECK (state IN ('open', 'closed', 'merged')),
    author          text,
    head_sha        text,
    merged_at       timestamptz,
    checks_state    text CHECK (checks_state IN ('pending', 'success', 'failure', 'neutral')),
    work_ref_id     uuid REFERENCES work_refs(id) ON DELETE SET NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repository_id, number)
);

CREATE INDEX pull_request_projections_open ON pull_request_projections (repository_id)
    WHERE state = 'open';

COMMIT;
