-- The role-to-action matrix, as data (wg-p4h.12, ADR-0026).
--
-- Nine roles and nineteen actions existed separately since 0001_core.sql, with
-- nothing connecting them. This is that connection, and it is the reason
-- internal/authz could not be called from the HTTP layer before now: there was
-- nothing to evaluate a grant against.
--
-- The rows below are GENERATED FROM the table in ADR-0026 rather than retyped,
-- so the document and the schema cannot drift by a transcription error. A test
-- re-derives them from the ADR and fails if they disagree.
--
-- Reviewed and accepted by a named human on 2026-08-29, including the eight
-- cells the ADR called out for a ruling. Changing a cell is a further ADR and a
-- migration, which is the correct cost for a permission change.

BEGIN;

CREATE TABLE role_permissions (
    role_name text NOT NULL REFERENCES roles(name) ON DELETE CASCADE,
    -- Not a foreign key: the action vocabulary lives in Go, as a closed set that
    -- authz.Action.Known() enforces. A second copy as a lookup table would be a
    -- second place for it to drift. The test that re-derives this from the ADR
    -- also asserts every action here is one authz recognises.
    action    text NOT NULL,
    PRIMARY KEY (role_name, action)
);

INSERT INTO role_permissions (role_name, action) VALUES
    ('organisation_admin', 'organisation.read'),
    ('executive', 'organisation.read'),
    ('portfolio_lead', 'organisation.read'),
    ('function_lead', 'organisation.read'),
    ('organisation_admin', 'project.read'),
    ('executive', 'project.read'),
    ('portfolio_lead', 'project.read'),
    ('function_lead', 'project.read'),
    ('project_lead', 'project.read'),
    ('backup_operator', 'project.read'),
    ('contributor', 'project.read'),
    ('observer', 'project.read'),
    ('external_client', 'project.read'),
    ('organisation_admin', 'project.manage'),
    ('portfolio_lead', 'project.manage'),
    ('function_lead', 'project.manage'),
    ('project_lead', 'project.manage'),
    ('organisation_admin', 'work.create'),
    ('portfolio_lead', 'work.create'),
    ('function_lead', 'work.create'),
    ('project_lead', 'work.create'),
    ('backup_operator', 'work.create'),
    ('contributor', 'work.create'),
    ('organisation_admin', 'work.update'),
    ('portfolio_lead', 'work.update'),
    ('function_lead', 'work.update'),
    ('project_lead', 'work.update'),
    ('backup_operator', 'work.update'),
    ('contributor', 'work.update'),
    ('organisation_admin', 'work.assign'),
    ('portfolio_lead', 'work.assign'),
    ('function_lead', 'work.assign'),
    ('project_lead', 'work.assign'),
    ('backup_operator', 'work.assign'),
    ('organisation_admin', 'agent.dispatch'),
    ('portfolio_lead', 'agent.dispatch'),
    ('function_lead', 'agent.dispatch'),
    ('project_lead', 'agent.dispatch'),
    ('backup_operator', 'agent.dispatch'),
    ('organisation_admin', 'agent.inspect'),
    ('executive', 'agent.inspect'),
    ('portfolio_lead', 'agent.inspect'),
    ('function_lead', 'agent.inspect'),
    ('project_lead', 'agent.inspect'),
    ('backup_operator', 'agent.inspect'),
    ('contributor', 'agent.inspect'),
    ('observer', 'agent.inspect'),
    ('organisation_admin', 'agent.stop'),
    ('portfolio_lead', 'agent.stop'),
    ('function_lead', 'agent.stop'),
    ('project_lead', 'agent.stop'),
    ('backup_operator', 'agent.stop'),
    ('organisation_admin', 'repository.read'),
    ('portfolio_lead', 'repository.read'),
    ('function_lead', 'repository.read'),
    ('project_lead', 'repository.read'),
    ('backup_operator', 'repository.read'),
    ('contributor', 'repository.read'),
    ('organisation_admin', 'pull_request.create'),
    ('portfolio_lead', 'pull_request.create'),
    ('function_lead', 'pull_request.create'),
    ('project_lead', 'pull_request.create'),
    ('backup_operator', 'pull_request.create'),
    ('contributor', 'pull_request.create'),
    ('organisation_admin', 'pull_request.merge'),
    ('portfolio_lead', 'pull_request.merge'),
    ('project_lead', 'pull_request.merge'),
    ('backup_operator', 'pull_request.merge'),
    ('executive', 'approval.decide'),
    ('portfolio_lead', 'approval.decide'),
    ('function_lead', 'approval.decide'),
    ('project_lead', 'approval.decide'),
    ('backup_operator', 'approval.decide'),
    ('external_client', 'approval.decide'),
    ('organisation_admin', 'deployment.execute'),
    ('project_lead', 'deployment.execute'),
    ('backup_operator', 'deployment.execute'),
    ('organisation_admin', 'secret.manage'),
    ('organisation_admin', 'policy.manage'),
    ('organisation_admin', 'audit.read'),
    ('executive', 'audit.read'),
    ('backup_operator', 'audit.read'),
    ('executive', 'marketing.publish'),
    ('function_lead', 'marketing.publish'),
    ('executive', 'knowledge.classification.downgrade');

ALTER TABLE role_permissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE role_permissions FORCE ROW LEVEL SECURITY;

-- The matrix is not secret. It is in an ADR in the repository, and a person who
-- cannot read which actions their own role carries cannot reason about a
-- refusal they receive. Readable by any authenticated user; writable by nobody
-- through the application, because changing it is a migration.
CREATE POLICY role_permissions_read ON role_permissions
    FOR SELECT
    USING (current_app_user() IS NOT NULL);

-- Whether a user holds an action, at organisation scope or for a project.
--
-- Mirrors authz.GrantSet's semantics exactly, which is what makes the in-memory
-- authorizer a faithful stand-in for tests: an organisation-scoped grant covers
-- every project, a project-scoped grant covers only its own, and an expired
-- grant covers nothing.
CREATE OR REPLACE FUNCTION system_user_may(
    p_user_id    uuid,
    p_action     text,
    p_project_id uuid DEFAULT NULL
) RETURNS boolean
LANGUAGE sql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
    SELECT EXISTS (
        SELECT 1
          FROM role_grants g
          JOIN role_permissions p ON p.role_name = g.role_name
         WHERE g.user_id = p_user_id
           AND p.action = p_action
           AND (g.expires_at IS NULL OR g.expires_at > now())
           AND (g.project_id IS NULL OR g.project_id = p_project_id)
    );
$$;

REVOKE ALL ON FUNCTION system_user_may(uuid, text, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_user_may(uuid, text, uuid) TO workgraph_app;
GRANT SELECT ON role_permissions TO workgraph_app;

COMMIT;
