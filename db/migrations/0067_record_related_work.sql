-- The record names the bead it produced (WP-H4).
--
-- The bead's description carries "Record: mem-<id>" and the record's front
-- matter said related_work: []. The link ran one way, and the two artefacts
-- exist so Git and PostgreSQL agree -- a decision you can reach from the work
-- graph but not the other way round is half of that.
--
-- RECORD-SCHEMA.md asks for the identity TUPLE rather than a bead id, because
-- an id alone is ambiguous between graphs (plan section 7.3). So this returns
-- one, and the renderer writes it.
BEGIN;

DROP FUNCTION IF EXISTS system_pending_record_publications(integer);

CREATE FUNCTION system_pending_record_publications(p_limit integer DEFAULT 20)
RETURNS TABLE (
    record_id      uuid,
    organisation_id uuid,
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
    supersedes_git_path text,
    sources        jsonb,
    candidate_id   uuid,
    markdown_published boolean,
    bead_published boolean,
    related_work   jsonb,
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
               prov.candidate_published,
               prov.related_work,
               old.git_path AS supersedes_git_path,
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
                     min(rs.candidate_id::text)::uuid AS candidate_id,
                     bool_or(c.work_ref_id IS NOT NULL) AS candidate_published,
                     -- The beads this record's candidates became, as the
                     -- identity tuple the schema asks for. A bead id alone is
                     -- ambiguous between graphs (plan section 7.3), which is
                     -- why related_work is a tuple and not a string.
                     jsonb_agg(DISTINCT jsonb_build_object(
                         'organisation_id', w.organisation_id,
                         'execution_cell_id', coalesce(w.execution_cell_id::text, ''),
                         'beads_database_id', w.beads_database_id,
                         'bead_id', w.bead_id))
                       FILTER (WHERE w.id IS NOT NULL) AS related_work
                FROM knowledge_record_sources rs
                JOIN knowledge_sources s ON s.id = rs.source_id
                LEFT JOIN knowledge_candidates c ON c.id = rs.candidate_id
                LEFT JOIN work_refs w ON w.id = c.work_ref_id
               WHERE rs.record_id = r.id
          ) prov ON true
          -- The graph a decision bead would go into, by the same rule the
          -- operational publisher uses: the project's graph, or the company
          -- graph when there is no project.
          -- The retired record's file, so a supersession can mark it in the
          -- same pull request. Only its front matter is touched; a body
          -- somebody wrote by hand is not regenerable and must survive.
          LEFT JOIN knowledge_records old ON old.id = r.supersedes_id
          LEFT JOIN LATERAL (
              SELECT b.id, b.name, b.path, b.host
                FROM beads_databases b
               WHERE CASE WHEN r.project_id IS NOT NULL
                          THEN b.project_id = r.project_id
                          ELSE b.scope = 'company' AND b.project_id IS NULL END
               ORDER BY (coalesce(b.path, '') = ''), b.name
               LIMIT 1
          ) g ON true
         WHERE r.status = 'accepted'
           -- Offered while EITHER effect is outstanding, not only the
           -- Markdown one. A decision whose pull request was opened and whose
           -- bead was not is still unfinished work, and keying the queue on
           -- git_path alone dropped it the moment the first half succeeded.
           AND (r.git_path IS NULL
                OR (r.record_type = 'decision'
                    AND EXISTS (SELECT 1 FROM knowledge_record_sources rs2
                                 JOIN knowledge_candidates c2 ON c2.id = rs2.candidate_id
                                WHERE rs2.record_id = r.id AND c2.work_ref_id IS NULL)))
    )
    SELECT id, organisation_id, record_type, statement, scope, project_slug, function_slug,
           visibility, confidence, valid_from, review_after, reviewed_at,
           reviewer_email, owner_email, author_email, human_authored,
           supersedes_id,
           supersedes_git_path,
           coalesce(sources, '[]'::jsonb),
           candidate_id,
           git_path IS NOT NULL,
           coalesce(candidate_published, false),
           coalesce(related_work, '[]'::jsonb),
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

REVOKE ALL ON FUNCTION system_pending_record_publications(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_pending_record_publications(integer) TO workgraph_app;

COMMIT;
