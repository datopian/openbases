-- system_bootstrap_organisation creates one tenant and refuses to create two.
--
-- Runs against a database that `migrate -fresh` installed and `wg-init` then
-- initialised, so the "created" path is already exercised by the tool itself.
-- What is asserted here is the shape of what it wrote, and the refusals -- which
-- are the reason this is a function and not four INSERT statements.
--
-- Owner-only by design, recorded in scripts/check_rls_tests.py: every write
-- here goes through a SECURITY DEFINER function that runs with no app user by
-- definition, because at bootstrap there is none. Dropping to workgraph_app
-- would make the function's own writes invisible and the test would fail for a
-- reason unrelated to what it checks. Who may READ these tables is covered by
-- rls_isolation.sql and management_access.sql.
\set ON_ERROR_STOP on

-- The tenant wg-init created.
DO $$
DECLARE
    v_org   uuid;
    v_user  uuid;
    v_by    uuid;
    n       integer;
BEGIN
    SELECT id INTO v_org FROM organisations WHERE slug = 'acme';
    IF v_org IS NULL THEN
        RAISE EXCEPTION 'wg-init did not create the acme organisation';
    END IF;

    SELECT count(*) INTO n FROM organisations;
    IF n <> 1 THEN
        RAISE EXCEPTION 'expected exactly one organisation, found %', n;
    END IF;

    SELECT id INTO v_user FROM users
     WHERE organisation_id = v_org AND primary_email = 'ops@acme.example';
    IF v_user IS NULL THEN
        RAISE EXCEPTION 'the first administrator was not created';
    END IF;

    -- The grant is the point. A user with no grant can sign in and do nothing,
    -- which looks like a broken deployment rather than a missing row.
    SELECT count(*) INTO n FROM role_grants
     WHERE user_id = v_user AND role_name = 'organisation_admin'
       AND organisation_id = v_org AND project_id IS NULL;
    IF n <> 1 THEN
        RAISE EXCEPTION 'expected one organisation_admin grant, found %', n;
    END IF;

    -- granted_by must be NULL: nobody granted this, the install did. Recording
    -- the new administrator as their own granter would be the thing AGENTS.md
    -- rule 9 forbids, written into the audit trail on day one.
    SELECT granted_by INTO v_by FROM role_grants
     WHERE user_id = v_user AND role_name = 'organisation_admin';
    IF v_by IS NOT NULL THEN
        RAISE EXCEPTION 'the bootstrap grant names a granter (%); nobody granted it', v_by;
    END IF;
END
$$;

-- Re-running with the same arguments changes nothing and says so. A deploy-time
-- tool that cannot be re-run is one people are afraid to use.
DO $$
DECLARE s text;
BEGIN
    SELECT system_bootstrap_organisation('acme', 'Acme Ltd', 'ops@acme.example', 'Dana Ops')
      INTO s;
    IF s <> 'already_initialised' THEN
        RAISE EXCEPTION 'a repeat bootstrap reported %, not already_initialised', s;
    END IF;
END
$$;

-- Case is not a second person. An address differing only in case must find the
-- same user, or the deployment ends up with two administrators for one human
-- and grants split between them.
DO $$
DECLARE s text; n integer;
BEGIN
    SELECT system_bootstrap_organisation('acme', 'Acme Ltd', 'OPS@ACME.EXAMPLE', 'Dana Ops')
      INTO s;
    SELECT count(*) INTO n FROM users WHERE lower(primary_email) = 'ops@acme.example';
    IF n <> 1 THEN
        RAISE EXCEPTION 'an address differing in case created % users', n;
    END IF;
END
$$;

-- A second organisation is refused. One company per deployment is documented,
-- and a bootstrap that could add a tenant would contradict it silently.
DO $$
DECLARE s text;
BEGIN
    SELECT system_bootstrap_organisation('other', 'Other Ltd', 'ops@other.example', 'Sam')
      INTO s;
    RAISE EXCEPTION 'a second organisation was accepted, reporting %', s;
EXCEPTION
    WHEN raise_exception THEN
        IF position('one company' IN SQLERRM) = 0
           AND position('already belongs' IN SQLERRM) = 0 THEN
            RAISE EXCEPTION 'refused for the wrong reason: %', SQLERRM;
        END IF;
END
$$;

-- An address that is not an address is refused. Accepting one creates an
-- administrator who can never sign in, in a deployment with nobody who can fix
-- it -- the failure is total and arrives later.
DO $$
DECLARE s text;
BEGIN
    SELECT system_bootstrap_organisation('acme', 'Acme Ltd', 'dana', 'Dana Ops') INTO s;
    RAISE EXCEPTION 'a bare username was accepted as an address, reporting %', s;
EXCEPTION
    WHEN raise_exception THEN
        IF position('identity provider' IN SQLERRM) = 0 THEN
            RAISE EXCEPTION 'refused for the wrong reason: %', SQLERRM;
        END IF;
END
$$;

-- An empty name is refused rather than stored. This runs once and the value is
-- what every screen shows afterwards.
DO $$
DECLARE s text;
BEGIN
    SELECT system_bootstrap_organisation('acme', '   ', 'ops@acme.example', 'Dana') INTO s;
    RAISE EXCEPTION 'a blank organisation name was accepted, reporting %', s;
EXCEPTION
    WHEN raise_exception THEN
        IF position('needs a name' IN SQLERRM) = 0 THEN
            RAISE EXCEPTION 'refused for the wrong reason: %', SQLERRM;
        END IF;
END
$$;

-- The whole reason this is SECURITY DEFINER: the application role must be able
-- to run it, and RLS on organisations, users and role_grants requires
-- current_app_user() IS NOT NULL, which at bootstrap is NULL by definition.
--
-- Everything above runs as the owner, who bypasses RLS, so none of it can tell
-- whether the definer seam works or is merely unnecessary. This drops to
-- workgraph_app -- the role a deployed wg-init actually connects as -- and
-- proves both the GRANT EXECUTE and that the function's own writes are not
-- blocked by the policies on the tables it writes.
DO $$
DECLARE s text;
BEGIN
    SET LOCAL ROLE workgraph_app;

    -- Proving the SET took effect. `SET LOCAL` outside a transaction is a
    -- warning that applies nothing, and without this assertion the call below
    -- would silently run as the owner and test nothing.
    IF current_user <> 'workgraph_app' THEN
        RAISE EXCEPTION 'expected to be workgraph_app, am %', current_user;
    END IF;

    SELECT system_bootstrap_organisation('acme', 'Acme Ltd', 'ops@acme.example', 'Dana Ops')
      INTO s;
    IF s <> 'already_initialised' THEN
        RAISE EXCEPTION 'as workgraph_app the bootstrap reported %, not already_initialised', s;
    END IF;
END
$$;

SELECT 'the bootstrap creates one tenant, is idempotent, refuses a second, and works as workgraph_app' AS result;
