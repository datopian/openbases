-- The bead is attributed to the reviewer who accepted it (WP-H4).
--
-- Found by reading the first bead publication created rather than by reasoning
-- about it. It said `Owner: workgraph-publisher`, because bd takes the owner
-- from the actor when no assignee is given -- so the Dolt commit trail and the
-- ownership of real work both pointed at a service account, and the reviewer
-- who made the decision appeared nowhere except in the description.
--
-- The reviewer was already carried by this function for exactly this purpose
-- and the publisher was not using it. Adding was_inferred is the other half of
-- the same problem: the description said "extracted by Workgraph" over every
-- statement, including one a person wrote by hand.
--
-- The function is dropped and recreated because a new output column changes
-- its return type, which CREATE OR REPLACE cannot do.
BEGIN;

DROP FUNCTION IF EXISTS system_pending_work_publications(integer);

CREATE FUNCTION system_pending_work_publications(p_limit integer DEFAULT 50)
RETURNS TABLE (
    candidate_id   uuid,
    candidate_type text,
    statement      text,
    due_date       date,
    visibility     text,
    confidence     numeric,
    was_inferred   boolean,
    project_slug   text,
    owner_email    text,
    reviewer_email text,
    source_id      uuid,
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
    WITH routed AS (
        SELECT c.id, c.candidate_type, c.statement, c.due_date, c.visibility,
               c.confidence, c.was_inferred, c.source_id, c.created_at,
               p.slug AS project_slug,
               u.primary_email AS owner_email,
               r.reviewer_email,
               g.id AS graph_id, g.name AS graph_name,
               g.path AS graph_path, g.host AS graph_host,
               CASE
                 -- A project's work goes in that project's graph or nowhere.
                 -- The company graph is not a fallback: routing a client's
                 -- accepted task there would put it in front of everyone at
                 -- the company.
                 WHEN c.proposed_project_id IS NOT NULL AND g.id IS NULL
                   THEN 'no graph is registered for project '
                        || coalesce(p.slug, c.proposed_project_id::text)
                 -- A restricted statement with no project has nowhere narrow
                 -- to go. Publishing it company-wide is the widening this
                 -- refuses to do quietly (ADR-0013).
                 WHEN c.proposed_project_id IS NULL AND c.visibility = 'restricted'
                   THEN 'restricted with no project: there is no graph narrow enough'
                 WHEN g.id IS NULL
                   THEN 'no company graph is registered'
                 -- Registered but not provisioned anywhere. A different fix
                 -- from having no graph at all -- run the registry against the
                 -- host that has it -- so it says so separately.
                 WHEN coalesce(g.path, '') = ''
                   THEN 'graph ' || g.name || ' is registered with no path'
                 ELSE NULL
               END AS blocked
          FROM knowledge_candidates c
          LEFT JOIN projects p ON p.id = c.proposed_project_id
          LEFT JOIN users u ON u.id = c.proposed_owner_user_id
          -- The reviewer who accepted it. Passed to `bd` as the actor, so the
          -- Dolt commit trail attributes the work to the person who decided it
          -- exists rather than to the service account.
          LEFT JOIN LATERAL (
              SELECT ru.primary_email AS reviewer_email
                FROM knowledge_reviews kr
                JOIN users ru ON ru.id = kr.reviewer_user_id
               WHERE kr.candidate_id = c.id
                 AND kr.decision IN ('accept', 'edit_and_accept')
               ORDER BY kr.decided_at DESC
               LIMIT 1
          ) r ON true
          -- The graph is chosen by the candidate's project, or by being the
          -- company graph when there is no project. LATERAL with a limit
          -- rather than a plain join, so a second graph registered for one
          -- project cannot multiply the row and publish the candidate twice --
          -- preferring one that has actually been provisioned.
          LEFT JOIN LATERAL (
              SELECT b.id, b.name, b.path, b.host
                FROM beads_databases b
               WHERE CASE WHEN c.proposed_project_id IS NOT NULL
                          THEN b.project_id = c.proposed_project_id
                          ELSE b.scope = 'company' AND b.project_id IS NULL END
               ORDER BY (coalesce(b.path, '') = ''), b.name
               LIMIT 1
          ) g ON true
         WHERE c.work_ref_id IS NULL
           AND c.status IN ('accepted', 'edited_accepted')
           AND c.candidate_type IN ('task', 'commitment', 'risk', 'market-signal', 'question')
    )
    SELECT id, candidate_type, statement, due_date, visibility, confidence,
           was_inferred, project_slug, owner_email, reviewer_email, source_id,
           -- A blocked row carries no graph. The reason names it where that
           -- helps, but handing back a path the caller must not write to
           -- leaves one `if` between a refusal and a client's task in the
           -- company graph.
           CASE WHEN blocked IS NULL THEN graph_id END,
           CASE WHEN blocked IS NULL THEN graph_name END,
           CASE WHEN blocked IS NULL THEN graph_path END,
           CASE WHEN blocked IS NULL THEN graph_host END,
           blocked
      FROM routed
     ORDER BY created_at
     LIMIT greatest(p_limit, 1);
$$;


REVOKE ALL ON FUNCTION system_pending_work_publications(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_pending_work_publications(integer) TO workgraph_app;

COMMIT;
