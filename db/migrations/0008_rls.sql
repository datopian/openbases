-- 0008_rls.sql — row-level security backstop.
-- Work package: WP-C1. Plan sections 8.2, 17.2.
--
-- Application-layer authorisation is the first gate. RLS is the backstop that
-- survives a forgotten WHERE clause: if a query reaches the database without
-- the right session identity, it returns nothing rather than everything.
--
-- The application connects as workgraph_app and sets workgraph.user_id per
-- request. The role is deliberately not the table owner, because an owner
-- bypasses RLS unless FORCE is set — and relying on remembering FORCE is the
-- kind of assumption this migration exists to remove.

BEGIN;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'workgraph_app') THEN
        CREATE ROLE workgraph_app NOLOGIN;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO workgraph_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO workgraph_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO workgraph_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO workgraph_app;

-- The current request's user, set by the application on each connection.
-- Returns NULL when unset, which denies rather than admits.
CREATE OR REPLACE FUNCTION current_app_user() RETURNS uuid AS $$
    SELECT NULLIF(current_setting('workgraph.user_id', true), '')::uuid;
$$ LANGUAGE sql STABLE;

-- Whether the current user may see a given project. Membership or an
-- organisation-scoped grant; nothing else.
CREATE OR REPLACE FUNCTION can_read_project(p_project_id uuid) RETURNS boolean AS $$
    SELECT current_app_user() IS NOT NULL
       AND p_project_id IS NOT NULL
       AND (
         EXISTS (
           SELECT 1 FROM project_memberships m
           WHERE m.project_id = p_project_id AND m.user_id = current_app_user()
         )
         OR EXISTS (
           SELECT 1 FROM role_grants g
           WHERE g.user_id = current_app_user()
             AND g.project_id IS NULL
             AND g.role_name IN ('organisation_admin', 'executive')
             AND (g.expires_at IS NULL OR g.expires_at > now())
         )
       );
$$ LANGUAGE sql STABLE;

-- Sensitive, project-scoped tables. Each is forced so that even the owner is
-- subject to the policy.
ALTER TABLE projects              ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects              FORCE ROW LEVEL SECURITY;
ALTER TABLE work_refs             ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_refs             FORCE ROW LEVEL SECURITY;
ALTER TABLE knowledge_sources     ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge_sources     FORCE ROW LEVEL SECURITY;
ALTER TABLE knowledge_candidates  ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge_candidates  FORCE ROW LEVEL SECURITY;
ALTER TABLE knowledge_records     ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge_records     FORCE ROW LEVEL SECURITY;
ALTER TABLE marketing_packets     ENABLE ROW LEVEL SECURITY;
ALTER TABLE marketing_packets     FORCE ROW LEVEL SECURITY;
ALTER TABLE record_embeddings     ENABLE ROW LEVEL SECURITY;
ALTER TABLE record_embeddings     FORCE ROW LEVEL SECURITY;
ALTER TABLE record_search         ENABLE ROW LEVEL SECURITY;
ALTER TABLE record_search         FORCE ROW LEVEL SECURITY;

-- Read predicates, named once so that the SELECT policy and the UPDATE/DELETE
-- policies cannot drift apart.
CREATE OR REPLACE FUNCTION can_read_source(p_project_id uuid, p_visibility text) RETURNS boolean AS $$
    SELECT current_app_user() IS NOT NULL
       AND ((p_project_id IS NULL AND p_visibility = 'internal')
            OR can_read_project(p_project_id));
$$ LANGUAGE sql STABLE;

-- IMPORTANT: policies are permissive and OR together, and a FOR ALL policy
-- also covers SELECT. Write policies are therefore scoped to INSERT, UPDATE and
-- DELETE explicitly. A FOR ALL write policy here would silently re-open every
-- read — which is exactly what test/integration/rls_isolation.sql caught.

-- projects
CREATE POLICY projects_read   ON projects FOR SELECT USING (can_read_project(id));
CREATE POLICY projects_insert ON projects FOR INSERT WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY projects_update ON projects FOR UPDATE
    USING (can_read_project(id)) WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY projects_delete ON projects FOR DELETE USING (can_read_project(id));

-- work_refs
CREATE POLICY work_refs_read   ON work_refs FOR SELECT
    USING (project_id IS NULL OR can_read_project(project_id));
CREATE POLICY work_refs_insert ON work_refs FOR INSERT WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY work_refs_update ON work_refs FOR UPDATE
    USING (project_id IS NULL OR can_read_project(project_id))
    WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY work_refs_delete ON work_refs FOR DELETE
    USING (project_id IS NULL OR can_read_project(project_id));

-- knowledge_sources: no project scope means company-internal.
CREATE POLICY knowledge_sources_read   ON knowledge_sources FOR SELECT
    USING (can_read_source(project_id, visibility));
CREATE POLICY knowledge_sources_insert ON knowledge_sources FOR INSERT
    WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY knowledge_sources_update ON knowledge_sources FOR UPDATE
    USING (can_read_source(project_id, visibility)) WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY knowledge_sources_delete ON knowledge_sources FOR DELETE
    USING (can_read_source(project_id, visibility));

-- knowledge_candidates inherit their source's reach. This is the row that stops
-- an unauthorised reviewer discovering a restricted meeting's text.
CREATE POLICY knowledge_candidates_read ON knowledge_candidates FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM knowledge_sources s
        WHERE s.id = knowledge_candidates.source_id
          AND can_read_source(s.project_id, s.visibility)
    ));
CREATE POLICY knowledge_candidates_insert ON knowledge_candidates FOR INSERT
    WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY knowledge_candidates_update ON knowledge_candidates FOR UPDATE
    USING (EXISTS (
        SELECT 1 FROM knowledge_sources s
        WHERE s.id = knowledge_candidates.source_id
          AND can_read_source(s.project_id, s.visibility)
    ))
    WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY knowledge_candidates_delete ON knowledge_candidates FOR DELETE
    USING (EXISTS (
        SELECT 1 FROM knowledge_sources s
        WHERE s.id = knowledge_candidates.source_id
          AND can_read_source(s.project_id, s.visibility)
    ));

-- knowledge_records
CREATE POLICY knowledge_records_read ON knowledge_records FOR SELECT
    USING (current_app_user() IS NOT NULL
           AND ((scope = 'company' AND visibility = 'internal') OR can_read_project(project_id)));
CREATE POLICY knowledge_records_insert ON knowledge_records FOR INSERT
    WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY knowledge_records_update ON knowledge_records FOR UPDATE
    USING (current_app_user() IS NOT NULL
           AND ((scope = 'company' AND visibility = 'internal') OR can_read_project(project_id)))
    WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY knowledge_records_delete ON knowledge_records FOR DELETE
    USING (current_app_user() IS NOT NULL
           AND ((scope = 'company' AND visibility = 'internal') OR can_read_project(project_id)));

-- marketing_packets
CREATE POLICY marketing_packets_read   ON marketing_packets FOR SELECT
    USING (can_read_project(project_id));
CREATE POLICY marketing_packets_insert ON marketing_packets FOR INSERT
    WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY marketing_packets_update ON marketing_packets FOR UPDATE
    USING (can_read_project(project_id)) WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY marketing_packets_delete ON marketing_packets FOR DELETE
    USING (can_read_project(project_id));

-- The retrieval projections carry the same filter as the records they index, so
-- a vector or keyword search cannot reach further than a direct read.
CREATE POLICY record_embeddings_read   ON record_embeddings FOR SELECT
    USING (can_read_source(project_id, visibility));
CREATE POLICY record_embeddings_insert ON record_embeddings FOR INSERT
    WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY record_embeddings_update ON record_embeddings FOR UPDATE
    USING (can_read_source(project_id, visibility)) WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY record_embeddings_delete ON record_embeddings FOR DELETE
    USING (can_read_source(project_id, visibility));

CREATE POLICY record_search_read   ON record_search FOR SELECT
    USING (can_read_source(project_id, visibility));
CREATE POLICY record_search_insert ON record_search FOR INSERT
    WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY record_search_update ON record_search FOR UPDATE
    USING (can_read_source(project_id, visibility)) WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY record_search_delete ON record_search FOR DELETE
    USING (can_read_source(project_id, visibility));

COMMIT;
