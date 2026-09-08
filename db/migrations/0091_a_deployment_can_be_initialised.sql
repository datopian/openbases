-- Stage 2 of docs/install/deployable-by-others.md: a deployment can create its
-- own first organisation and administrator.
--
-- `migrate -fresh` leaves a database with the schema and nobody in it, which is
-- correct and unusable: every read path resolves the caller to a user row and
-- refuses when there is none, so with no users nobody can sign in and there is
-- no way in to create the first one. Something has to write that row without
-- being a user, and it cannot be an authenticated request.
--
-- Why a function rather than plain INSERTs from the tool: RLS on users,
-- organisations and role_grants requires current_app_user() IS NOT NULL, and at
-- bootstrap there is no app user by definition. wg-init connects as the
-- application role like every other node-side tool, so it needs a
-- SECURITY DEFINER seam. Superuser would also work and is worse -- it would put
-- a second, unconstrained write path into the install story.
--
-- The refusal is the design. This creates an organisation only when the
-- deployment has none, and refuses outright when a DIFFERENT one exists. One
-- company per deployment is the documented stance, and a bootstrap that could
-- add a second organisation would quietly contradict it. Re-running with the
-- same arguments is free, because a deploy-time tool that cannot be re-run is
-- one people are afraid to use.
--
-- No identity row is created, deliberately. A Cloudflare Access subject is
-- issued by the provider and cannot be known in advance; internal/domain's
-- resolver links a subject to a user by email address on first sight. Creating
-- the user with the right primary_email is exactly what makes that work, and
-- inventing a subject here would create a row that never matches anybody.
BEGIN;

-- check_function_signatures.py: drop what we are about to create.
DROP FUNCTION IF EXISTS system_bootstrap_organisation(text, text, text, text, boolean);

CREATE OR REPLACE FUNCTION system_bootstrap_organisation(
    p_org_slug       text,
    p_org_name       text,
    p_admin_email    text,
    p_admin_name     text,
    p_with_portfolios boolean DEFAULT false
) RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_org         uuid;
    v_existing    text;
    v_count       integer;
    v_user        uuid;
    v_made_org    boolean := false;
    v_made_user   boolean := false;
    v_made_grant  boolean := false;
BEGIN
    IF coalesce(trim(p_org_slug), '') = '' THEN
        RAISE EXCEPTION 'an organisation needs a slug';
    END IF;
    IF coalesce(trim(p_org_name), '') = '' THEN
        RAISE EXCEPTION 'an organisation needs a name';
    END IF;
    IF coalesce(trim(p_admin_name), '') = '' THEN
        RAISE EXCEPTION 'the first administrator needs a display name';
    END IF;
    -- Not a full address grammar, which is a losing game. This catches the
    -- mistakes that matter: an empty value, a username, a typo'd domain. The
    -- address has to match what the identity provider will assert or the
    -- administrator can never sign in, and THAT is not checkable here.
    IF p_admin_email IS NULL OR p_admin_email NOT LIKE '%_@%_.%_' THEN
        RAISE EXCEPTION 'the first administrator needs an email address that the '
            'identity provider will assert, got %', coalesce(p_admin_email, '<null>');
    END IF;

    SELECT count(*) INTO v_count FROM organisations;
    IF v_count > 1 THEN
        RAISE EXCEPTION 'this deployment holds % organisations; it was not initialised by '
            'this function and cannot be repaired by it', v_count;
    END IF;

    SELECT id, slug INTO v_org, v_existing FROM organisations LIMIT 1;

    IF v_org IS NULL THEN
        INSERT INTO organisations (slug, name)
        VALUES (trim(p_org_slug), trim(p_org_name))
        RETURNING id INTO v_org;
        v_made_org := true;
    ELSIF v_existing <> trim(p_org_slug) THEN
        RAISE EXCEPTION 'this deployment already belongs to organisation %; one company '
            'per deployment, so initialising it as % is refused',
            v_existing, trim(p_org_slug);
    END IF;

    -- Matched case-insensitively: an address differing only in case is the same
    -- person, and creating a second user for them would split their grants.
    SELECT id INTO v_user
      FROM users
     WHERE organisation_id = v_org
       AND lower(primary_email) = lower(trim(p_admin_email));

    IF v_user IS NULL THEN
        INSERT INTO users (organisation_id, display_name, primary_email)
        VALUES (v_org, trim(p_admin_name), lower(trim(p_admin_email)))
        RETURNING id INTO v_user;
        v_made_user := true;
    END IF;

    -- granted_by stays NULL, which is the honest record: nobody granted this,
    -- the install did. AGENTS.md rule 9 says nothing approves itself, and
    -- naming the new administrator as their own granter would be exactly that.
    INSERT INTO role_grants (user_id, role_name, organisation_id)
    VALUES (v_user, 'organisation_admin', v_org)
    ON CONFLICT DO NOTHING;
    v_made_grant := FOUND;

    IF p_with_portfolios THEN
        INSERT INTO portfolios (organisation_id, slug, name, kind)
        SELECT v_org, v.slug, v.name, v.kind
          FROM (VALUES
                  ('oss',      'Open Source',        'oss'),
                  ('product',  'Products',           'product'),
                  ('client',   'Client Engagements', 'client'),
                  ('internal', 'Internal',           'internal')
               ) AS v(slug, name, kind)
        ON CONFLICT (organisation_id, slug) DO NOTHING;
    END IF;

    IF v_made_org THEN
        RETURN 'created';
    ELSIF v_made_user OR v_made_grant THEN
        RETURN 'admin_added';
    END IF;
    RETURN 'already_initialised';
END
$$;

GRANT EXECUTE ON FUNCTION system_bootstrap_organisation(text, text, text, text, boolean) TO workgraph_app;

COMMIT;
