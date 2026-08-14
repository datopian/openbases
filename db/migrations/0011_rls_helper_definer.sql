-- 0011_rls_helper_definer.sql — break RLS recursion in the permission helpers.
-- Work package: WP-C3. Fixes a defect introduced by 0010.
--
-- 0010 put row-level security on project_memberships and role_grants. Those are
-- the tables can_read_project() reads to decide access, so evaluating a policy
-- called the function, which queried a table, which evaluated a policy, which
-- called the function. Every query on a protected table failed.
--
-- The fix is to run the helpers as SECURITY DEFINER. They are owned by a
-- superuser, and superusers bypass row-level security, so the membership lookup
-- inside the helper is not itself filtered.
--
-- That is safe here because of what these functions can return: a boolean about
-- the CURRENT caller's own access, derived from a session setting the caller
-- does not control. They expose no row and take no argument that could widen
-- their own answer.
--
-- search_path is pinned on each. A SECURITY DEFINER function without it can be
-- hijacked by a caller who puts a malicious table earlier in their search path,
-- which would turn this fix into a privilege escalation.

BEGIN;

CREATE OR REPLACE FUNCTION current_app_user() RETURNS uuid
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = public, pg_temp
AS $$
    SELECT NULLIF(current_setting('workgraph.user_id', true), '')::uuid;
$$;

CREATE OR REPLACE FUNCTION can_read_project(p_project_id uuid) RETURNS boolean
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = public, pg_temp
AS $$
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
$$;

CREATE OR REPLACE FUNCTION can_read_project_row(p_project_id uuid) RETURNS boolean
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = public, pg_temp
AS $$
    SELECT current_app_user() IS NOT NULL
       AND (p_project_id IS NULL OR can_read_project(p_project_id));
$$;

-- The application role may call them but must not be able to redefine them.
REVOKE ALL ON FUNCTION current_app_user() FROM PUBLIC;
REVOKE ALL ON FUNCTION can_read_project(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION can_read_project_row(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION current_app_user() TO workgraph_app;
GRANT EXECUTE ON FUNCTION can_read_project(uuid) TO workgraph_app;
GRANT EXECUTE ON FUNCTION can_read_project_row(uuid) TO workgraph_app;

COMMIT;
