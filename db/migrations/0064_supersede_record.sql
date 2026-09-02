-- Correcting company memory means superseding, never overwriting (WP-H4).
--
-- plan section 14.7 and knowledge/RECORD-SCHEMA.md rule 3 say the same thing in
-- different words: "Supersede, never overwrite. Correcting a record means
-- publishing a new record with supersedes: set and marking the old one status:
-- superseded with superseded_by:. History is evidence."
--
-- 0002 put the columns and the constraints in place -- a superseded record must
-- name its successor -- and nothing could reach them. There was no way to
-- correct an accepted record at all, which in practice means the first way
-- somebody corrects one is an UPDATE, and the evidence is gone.
BEGIN;

-- Supersede a record with a corrected statement.
--
-- Invoker rights, deliberately. This is a human act on company memory, so it
-- runs as the reviewer and RLS decides whether they may see the record at all;
-- a SECURITY DEFINER version would let anybody with EXECUTE rewrite any
-- project's memory.
CREATE FUNCTION supersede_record(
    p_record_id uuid,
    p_statement text,
    p_reason    text
) RETURNS jsonb
LANGUAGE plpgsql
AS $$
DECLARE
    v_user  uuid := current_app_user();
    v_old   knowledge_records%ROWTYPE;
    v_new   uuid;
    v_after date;
BEGIN
    IF v_user IS NULL THEN
        RAISE EXCEPTION 'superseding a record needs a named reviewer';
    END IF;
    IF coalesce(trim(p_statement), '') = '' THEN
        RAISE EXCEPTION 'a superseding record needs a statement';
    END IF;
    IF coalesce(trim(p_reason), '') = '' THEN
        -- Required, not optional. The reason IS the audit history this work
        -- package is asked to preserve: "the old one was wrong" without saying
        -- how is a record of an edit, not of a correction.
        RAISE EXCEPTION 'superseding a record needs a reason';
    END IF;

    -- Under RLS: a record the caller cannot read is a record they cannot
    -- supersede, and it fails as "no such record" rather than leaking that it
    -- exists.
    SELECT * INTO v_old FROM knowledge_records WHERE id = p_record_id;
    IF v_old.id IS NULL THEN
        RAISE EXCEPTION 'no such record %', p_record_id;
    END IF;
    IF v_old.status <> 'accepted' THEN
        RAISE EXCEPTION 'record % is %, so there is nothing live to supersede',
            p_record_id, v_old.status;
    END IF;

    v_after := CASE v_old.record_type
        WHEN 'fact'       THEN current_date + 180
        WHEN 'constraint' THEN current_date + 365
        WHEN 'assumption' THEN current_date + 90
        ELSE NULL END;

    -- The successor inherits type, scope and classification. Changing any of
    -- those is a different record rather than a correction of this one, and
    -- inheriting the classification is what stops a correction being the way
    -- restricted material becomes internal.
    INSERT INTO knowledge_records (
        organisation_id, record_type, statement, scope, project_id, function_id,
        owner_user_id, reviewer_user_id, visibility, confidence, review_after,
        supersedes_id, status,
        -- A correction is somebody's own words, so the successor is
        -- human-authored with them as the author. Its provenance is copied
        -- below as well: the sources that led to the original claim are still
        -- why the subject came up.
        human_authored, author_user_id
    ) VALUES (
        v_old.organisation_id, v_old.record_type, trim(p_statement), v_old.scope,
        v_old.project_id, v_old.function_id, v_old.owner_user_id, v_user,
        v_old.visibility, v_old.confidence, v_after, v_old.id, 'accepted',
        true, v_user
    ) RETURNING id INTO v_new;

    INSERT INTO knowledge_record_sources (record_id, source_id, candidate_id)
    SELECT v_new, source_id, candidate_id
      FROM knowledge_record_sources WHERE record_id = v_old.id;

    -- Retired, not deleted. The CHECK refuses 'superseded' without a
    -- successor, so these two are one statement on purpose.
    UPDATE knowledge_records
       SET status = 'superseded', superseded_by_id = v_new
     WHERE id = v_old.id;

    INSERT INTO audit_log (actor_user_id, action, target_type, target_id,
                           project_id, outcome, reason, evidence_refs)
    VALUES (v_user, 'knowledge.record.supersede', 'knowledge_record',
            v_old.id::text, v_old.project_id, 'executed', trim(p_reason),
            ARRAY['record:' || v_new::text]);

    RETURN jsonb_build_object(
        'superseded', v_old.id,
        'record_id', v_new,
        -- The path of the retired file, when it has one. The publisher needs
        -- it to mark that file superseded in the same pull request as the new
        -- one, without rewriting a body somebody wrote by hand.
        'superseded_git_path', v_old.git_path);
END
$$;

REVOKE ALL ON FUNCTION supersede_record(uuid, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION supersede_record(uuid, text, text) TO workgraph_app;

-- The publisher needs to know, for a record that supersedes another, where the
-- retired file is. Added to the pending query rather than fetched separately so
-- one read decides everything a proposal does.
DROP FUNCTION IF EXISTS system_pending_record_publications(integer);

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
    supersedes_git_path text,
    sources        jsonb,
    candidate_id   uuid,
    markdown_published boolean,
    bead_published boolean,
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
                     bool_or(c.work_ref_id IS NOT NULL) AS candidate_published
                FROM knowledge_record_sources rs
                JOIN knowledge_sources s ON s.id = rs.source_id
                LEFT JOIN knowledge_candidates c ON c.id = rs.candidate_id
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
    SELECT id, record_type, statement, scope, project_slug, function_slug,
           visibility, confidence, valid_from, review_after, reviewed_at,
           reviewer_email, owner_email, author_email, human_authored,
           supersedes_id,
           supersedes_git_path,
           coalesce(sources, '[]'::jsonb),
           candidate_id,
           git_path IS NOT NULL,
           coalesce(candidate_published, false),
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
