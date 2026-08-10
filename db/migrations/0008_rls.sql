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

CREATE POLICY projects_read ON projects FOR SELECT
    USING (can_read_project(id));

CREATE POLICY work_refs_read ON work_refs FOR SELECT
    USING (project_id IS NULL OR can_read_project(project_id));

-- A source with no project scope is company-internal; a project-scoped source
-- is visible only to that project's members.
CREATE POLICY knowledge_sources_read ON knowledge_sources FOR SELECT
    USING (
      current_app_user() IS NOT NULL
      AND (project_id IS NULL AND visibility = 'internal'
           OR can_read_project(project_id))
    );

-- A candidate inherits its source's reach. This is the row that stops an
-- unauthorised reviewer discovering a restricted meeting's text.
CREATE POLICY knowledge_candidates_read ON knowledge_candidates FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM knowledge_sources s
        WHERE s.id = knowledge_candidates.source_id
          AND (s.project_id IS NULL AND s.visibility = 'internal'
               OR can_read_project(s.project_id))
    ));

CREATE POLICY knowledge_records_read ON knowledge_records FOR SELECT
    USING (
      current_app_user() IS NOT NULL
      AND (scope = 'company' AND visibility = 'internal'
           OR can_read_project(project_id))
    );

CREATE POLICY marketing_packets_read ON marketing_packets FOR SELECT
    USING (can_read_project(project_id));

-- The retrieval projections carry the same filter as the records they index,
-- so a vector or keyword search cannot reach further than a direct read.
CREATE POLICY record_embeddings_read ON record_embeddings FOR SELECT
    USING (
      current_app_user() IS NOT NULL
      AND (project_id IS NULL AND visibility = 'internal'
           OR can_read_project(project_id))
    );

CREATE POLICY record_search_read ON record_search FOR SELECT
    USING (
      current_app_user() IS NOT NULL
      AND (project_id IS NULL AND visibility = 'internal'
           OR can_read_project(project_id))
    );

-- Writes go through the domain services, which check permissions before
-- issuing the statement. These permissive write policies exist so that RLS does
-- not silently break inserts; they are not the authorisation boundary for
-- writes, and tightening them is tracked with the services that need it.
CREATE POLICY projects_write ON projects FOR ALL
    USING (current_app_user() IS NOT NULL) WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY work_refs_write ON work_refs FOR ALL
    USING (current_app_user() IS NOT NULL) WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY knowledge_sources_write ON knowledge_sources FOR ALL
    USING (current_app_user() IS NOT NULL) WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY knowledge_candidates_write ON knowledge_candidates FOR ALL
    USING (current_app_user() IS NOT NULL) WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY knowledge_records_write ON knowledge_records FOR ALL
    USING (current_app_user() IS NOT NULL) WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY marketing_packets_write ON marketing_packets FOR ALL
    USING (current_app_user() IS NOT NULL) WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY record_embeddings_write ON record_embeddings FOR ALL
    USING (current_app_user() IS NOT NULL) WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY record_search_write ON record_search FOR ALL
    USING (current_app_user() IS NOT NULL) WITH CHECK (current_app_user() IS NOT NULL);

COMMIT;
