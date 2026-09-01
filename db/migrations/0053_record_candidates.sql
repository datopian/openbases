-- Storing extracted candidates (WP-H3, plan section 14.4).
--
-- SECURITY DEFINER for the usual reason: extraction runs on a timer with no
-- user and knowledge_candidates is behind row-level security.
BEGIN;

-- Record the candidates extracted from one source, replacing any previous set
-- for the same extractor version.
--
-- Replaced rather than appended, and scoped to PENDING rows only. Re-running an
-- extractor must not duplicate a reviewer's queue, and must never touch a
-- candidate somebody has already decided on -- a rejected candidate that comes
-- back on the next pass is the fastest way to make people stop reviewing.
CREATE OR REPLACE FUNCTION system_record_candidates(
    p_source_id  uuid,
    p_version    text,
    p_candidates jsonb
) RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_visibility text;
    v_project    uuid;
    n            integer;
BEGIN
    SELECT visibility, project_id INTO v_visibility, v_project
      FROM knowledge_sources WHERE id = p_source_id;
    IF v_visibility IS NULL THEN
        RAISE EXCEPTION 'no such source %', p_source_id;
    END IF;
    IF jsonb_typeof(p_candidates) <> 'array' THEN
        RAISE EXCEPTION 'expected a JSON array of candidates, got %', jsonb_typeof(p_candidates);
    END IF;

    DELETE FROM knowledge_candidates
     WHERE source_id = p_source_id
       AND status = 'pending'
       AND extractor_prompt_version IS NOT DISTINCT FROM p_version;

    INSERT INTO knowledge_candidates (
        source_id, candidate_type, statement, due_date, confidence,
        -- Visibility is taken from the SOURCE and is not readable from the
        -- payload. The extractor does not get a say: a model that could set
        -- its own output's classification could downgrade restricted material
        -- by emitting a field, and ADR-0013 makes inheritance the rule.
        visibility,
        proposed_project_id, source_spans, was_inferred, extractor_prompt_version
    )
    SELECT p_source_id,
           c ->> 'type',
           c ->> 'statement',
           NULLIF(c ->> 'due_date', '')::date,
           (c ->> 'confidence')::numeric,
           v_visibility,
           v_project,
           c -> 'source_spans',
           coalesce((c ->> 'inferred')::boolean, true),
           p_version
      FROM jsonb_array_elements(p_candidates) AS c;

    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END
$$;

-- The review queue. Ordered oldest first so a backlog drains in the order it
-- arrived rather than by whatever the planner chose.
CREATE OR REPLACE FUNCTION system_pending_candidates(p_limit integer DEFAULT 100)
RETURNS TABLE (
    candidate_id   uuid,
    source_id      uuid,
    candidate_type text,
    statement      text,
    confidence     numeric,
    visibility     text,
    source_spans   jsonb,
    project_slug   text,
    created_at     timestamptz
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT c.id, c.source_id, c.candidate_type, c.statement, c.confidence,
           c.visibility, c.source_spans, p.slug, c.created_at
      FROM knowledge_candidates c
      LEFT JOIN projects p ON p.id = c.proposed_project_id
     WHERE c.status = 'pending'
     ORDER BY c.created_at
     LIMIT greatest(1, least(coalesce(p_limit, 100), 500));
$$;

REVOKE ALL ON FUNCTION system_record_candidates(uuid, text, jsonb) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_pending_candidates(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_record_candidates(uuid, text, jsonb) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_pending_candidates(integer) TO workgraph_app;

COMMIT;
