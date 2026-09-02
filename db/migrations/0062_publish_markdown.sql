-- An accepted durable record becomes a Markdown pull request (WP-H4, plan 14.5).
--
-- The record is already in the database; this is the other half of the same
-- fact, in Git, where a human can read it in a diff and argue with it before it
-- becomes company memory. knowledge/RECORD-SCHEMA.md in company-workgraph is
-- the shape, and it exists so "Git and PostgreSQL never disagree about
-- provenance, classification, or freshness".
--
-- Like the bead publication, this cannot happen in the review transaction: it
-- means calling GitHub. So the database offers what is unpublished and records
-- what came back, and a pass of workspaced does the work.
BEGIN;

ALTER TABLE knowledge_records ADD COLUMN git_pr_url text;

COMMENT ON COLUMN knowledge_records.git_path IS
  'Path of the Markdown record in company-workgraph, set when the pull request is opened (WP-H4).';
COMMENT ON COLUMN knowledge_records.git_pr_url IS
  'The pull request proposing the Markdown record. Present with git_path or not at all.';

-- Present together or not at all: a path with no pull request is a file nobody
-- proposed, and a pull request with no path is unfindable.
ALTER TABLE knowledge_records ADD CONSTRAINT git_path_names_its_pull_request
    CHECK (num_nonnulls(git_path, git_pr_url) <> 1);

CREATE INDEX knowledge_records_awaiting_git
    ON knowledge_records (created_at)
 WHERE git_path IS NULL AND status = 'accepted';

-- What is accepted and not yet proposed in Git, with everything the renderer
-- needs to write the front matter.
--
-- Provenance is aggregated here rather than fetched per record by the caller:
-- the schema requires either a non-empty sources list or human_authored with a
-- named author, and assembling that in one query is what makes the rule
-- checkable in one place.
--
-- The spans are references -- provider revision and line number -- and never
-- the excerpt text. WP-H4's criterion is that the raw transcript is not
-- committed, and the cheapest way to keep that true is for the text never to
-- reach the renderer.
CREATE FUNCTION system_pending_record_publications(p_limit integer DEFAULT 20)
RETURNS TABLE (
    record_id      uuid,
    record_type    text,
    statement      text,
    scope          text,
    project_slug   text,
    function_slug  text,
    visibility     text,
    confidence     numeric,
    valid_from     date,
    review_after   date,
    reviewed_at    timestamptz,
    reviewer_email text,
    owner_email    text,
    author_email   text,
    human_authored boolean,
    supersedes     uuid,
    sources        jsonb,
    candidate_id   uuid,
    graph_id       uuid,
    graph_name     text,
    graph_path     text,
    graph_host     text,
    blocked_reason text
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    WITH pending AS (
        SELECT r.*,
               p.slug AS project_slug,
               f.slug AS function_slug,
               ru.primary_email AS reviewer_email,
               ou.primary_email AS owner_email,
               au.primary_email AS author_email,
               prov.sources,
               prov.candidate_id,
               g.id AS graph_id, g.name AS graph_name,
               g.path AS graph_path, g.host AS graph_host
          FROM knowledge_records r
          LEFT JOIN projects p ON p.id = r.project_id
          LEFT JOIN functions f ON f.id = r.function_id
          LEFT JOIN users ru ON ru.id = r.reviewer_user_id
          LEFT JOIN users ou ON ou.id = r.owner_user_id
          LEFT JOIN users au ON au.id = r.author_user_id
          LEFT JOIN LATERAL (
              SELECT jsonb_agg(jsonb_build_object(
                         'source_id', s.provider_source_id,
                         'provider', s.provider,
                         'provider_revision', coalesce(s.provider_revision, ''),
                         -- Line references, not text.
                         'excerpt_refs', coalesce((
                             SELECT jsonb_agg('line:' || (span ->> 'line'))
                               FROM jsonb_array_elements(c.source_spans) span
                              WHERE span ? 'line'), '[]'::jsonb))
                       ORDER BY s.provider_source_id) AS sources,
                     min(rs.candidate_id::text)::uuid AS candidate_id
                FROM knowledge_record_sources rs
                JOIN knowledge_sources s ON s.id = rs.source_id
                LEFT JOIN knowledge_candidates c ON c.id = rs.candidate_id
               WHERE rs.record_id = r.id
          ) prov ON true
          -- The graph a decision bead would go into, by the same rule the
          -- operational publisher uses: the project's graph, or the company
          -- graph when there is no project.
          LEFT JOIN LATERAL (
              SELECT b.id, b.name, b.path, b.host
                FROM beads_databases b
               WHERE CASE WHEN r.project_id IS NOT NULL
                          THEN b.project_id = r.project_id
                          ELSE b.scope = 'company' AND b.project_id IS NULL END
               ORDER BY (coalesce(b.path, '') = ''), b.name
               LIMIT 1
          ) g ON true
         WHERE r.git_path IS NULL
           AND r.status = 'accepted'
    )
    SELECT id, record_type, statement, scope, project_slug, function_slug,
           visibility, confidence, valid_from, review_after, reviewed_at,
           reviewer_email, owner_email, author_email, human_authored,
           supersedes_id,
           coalesce(sources, '[]'::jsonb),
           candidate_id,
           graph_id, graph_name, graph_path, graph_host,
           CASE
             -- Provenance is mandatory (RECORD-SCHEMA.md rule 1). A record with
             -- neither sources nor a named author is invalid, and committing it
             -- would put an unattributable claim in company memory.
             WHEN coalesce(jsonb_array_length(sources), 0) = 0
                  AND NOT (human_authored AND author_email IS NOT NULL)
               THEN 'no provenance: neither a source nor a named author'
             WHEN reviewer_email IS NULL
               THEN 'the reviewer has no email address to attribute the record to'
             WHEN scope = 'project' AND project_slug IS NULL
               THEN 'project-scoped with no project'
             WHEN scope = 'function' AND function_slug IS NULL
               THEN 'function-scoped with no function'
             ELSE NULL
           END
      FROM pending
     ORDER BY created_at
     LIMIT greatest(p_limit, 1);
$$;

-- Record where the Markdown went.
--
-- Idempotent: called again for a record that already has a path it returns
-- false and changes nothing, so a pass that opened the pull request and then
-- failed does not open a second one.
CREATE FUNCTION system_record_markdown_publication(
    p_record_id uuid,
    p_git_path  text,
    p_pr_url    text
) RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_existing text;
BEGIN
    IF coalesce(trim(p_git_path), '') = '' OR coalesce(trim(p_pr_url), '') = '' THEN
        RAISE EXCEPTION 'a Markdown publication needs both a path and a pull request';
    END IF;

    SELECT git_path INTO v_existing FROM knowledge_records WHERE id = p_record_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no such record %', p_record_id;
    END IF;
    IF v_existing IS NOT NULL THEN
        RETURN false;
    END IF;

    UPDATE knowledge_records
       SET git_path = trim(p_git_path), git_pr_url = trim(p_pr_url)
     WHERE id = p_record_id;
    RETURN true;
END
$$;

REVOKE ALL ON FUNCTION system_pending_record_publications(integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_record_markdown_publication(uuid, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_pending_record_publications(integer) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_record_markdown_publication(uuid, text, text) TO workgraph_app;

COMMIT;
