-- 0010_rls_coverage.sql — extend row-level security to every project-scoped table.
-- Work package: WP-C3. Plan sections 8.2, 17.2. Fixes a gap in 0008_rls.sql.
--
-- 0008 protected eight tables. An audit prompted by a failing WP-C3 acceptance
-- test found fifteen more carrying project_id with no policy at all, so an
-- authenticated non-member could read them.
--
-- The leak that surfaced it: project_repositories was fully readable, and a
-- repository name identifies the client. ADR-0013 is about exactly this — the
-- name of a restricted engagement is confidential even when its contents are
-- unreachable.
--
-- Policies are generated from an explicit table list rather than written out
-- sixty times. The list is the reviewable part; generating the shape once means
-- fifteen tables cannot drift apart, which is how the original eight and these
-- fifteen came to differ in the first place.

BEGIN;

-- Readable when the row belongs to a project the caller may read. A NULL
-- project_id means company scope, visible to any authenticated user — the same
-- rule 0008 applies to knowledge_sources.
CREATE OR REPLACE FUNCTION can_read_project_row(p_project_id uuid) RETURNS boolean AS $$
    SELECT current_app_user() IS NOT NULL
       AND (p_project_id IS NULL OR can_read_project(p_project_id));
$$ LANGUAGE sql STABLE;

DO $$
DECLARE
    t text;
    -- Every table carrying project_id that 0008 did not cover.
    --
    -- audit_log is deliberately included: an audit entry names the project it
    -- concerns, and plan section 8.2 gates audit reading behind a permission
    -- rather than making it universal. Organisation admins still see everything
    -- through their organisation-scoped grant.
    tables text[] := ARRAY[
        'project_repositories',
        'project_memberships',
        'beads_databases',
        'agent_profiles',
        'agent_runs',
        'approval_requests',
        'attention_items',
        'context_packs',
        'events',
        'marketing_signals',
        'narrative_snapshots',
        'budget_limits',
        'usage_records',
        'audit_log'
    ];
BEGIN
    FOREACH t IN ARRAY tables LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        -- FORCE so the table owner is subject to the policy too. Without it,
        -- anything connecting as the owner bypasses every rule below.
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);

        EXECUTE format(
            'CREATE POLICY %I ON %I FOR SELECT USING (can_read_project_row(project_id))',
            t || '_read', t);

        -- Writes are scoped to INSERT, UPDATE and DELETE explicitly. A FOR ALL
        -- policy would also cover SELECT and silently re-open every read beside
        -- it — the bug caught during review of 0008.
        EXECUTE format(
            'CREATE POLICY %I ON %I FOR INSERT WITH CHECK (current_app_user() IS NOT NULL)',
            t || '_insert', t);
        EXECUTE format(
            'CREATE POLICY %I ON %I FOR UPDATE USING (can_read_project_row(project_id)) '
            || 'WITH CHECK (current_app_user() IS NOT NULL)',
            t || '_update', t);
        EXECUTE format(
            'CREATE POLICY %I ON %I FOR DELETE USING (can_read_project_row(project_id))',
            t || '_delete', t);
    END LOOP;
END
$$;

-- role_grants needs its own rule: a person must be able to see their own
-- grants, including organisation-scoped ones that name no project.
ALTER TABLE role_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE role_grants FORCE ROW LEVEL SECURITY;

CREATE POLICY role_grants_read ON role_grants FOR SELECT
    USING (user_id = current_app_user() OR can_read_project_row(project_id));
CREATE POLICY role_grants_insert ON role_grants FOR INSERT
    WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY role_grants_update ON role_grants FOR UPDATE
    USING (user_id = current_app_user() OR can_read_project_row(project_id))
    WITH CHECK (current_app_user() IS NOT NULL);
CREATE POLICY role_grants_delete ON role_grants FOR DELETE
    USING (user_id = current_app_user() OR can_read_project_row(project_id));

COMMIT;
