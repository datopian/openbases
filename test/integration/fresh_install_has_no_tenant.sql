-- A fresh install contains nobody.
--
-- Run against a database created with `workgraph-migrate -fresh`, which records
-- the migrations in db/tenant_seeds.txt as applied without running them. What
-- must come up is the schema and the permission model, and nothing about any
-- person, company, project or repository.
--
-- This is the test that keeps the repository honest about being open source.
-- Without it the next seeding migration reintroduces Datopian into every
-- deployment, and the first to notice is somebody else installing it.
-- cmd/migrate's tests refuse a NEW seeding migration; this asserts the outcome
-- against a real database, which is the claim that actually matters.
--
-- Runs as the owner, deliberately, and needs no entry in
-- scripts/check_rls_tests.py: it counts through dynamic SQL, so that checker
-- does not see a table name here at all. The reason it SHOULD run as owner is
-- the point -- RLS is irrelevant to proving a table is empty, and dropping to
-- workgraph_app would weaken the test rather than strengthen it, because a
-- policy that hid rows would make an install that still contains Datopian look
-- clean. Superuser is the right lens for "nothing is here".
\set ON_ERROR_STOP on

DO $$
DECLARE
    t    text;
    n    bigint;
    bad  text := '';
BEGIN
    -- Every table that holds a record about the real world. `roles` is absent
    -- deliberately: it is the permission model, seeded by 0001_core.sql as
    -- schema, and it MUST be populated -- asserted separately below.
    FOREACH t IN ARRAY ARRAY[
        'organisations', 'users', 'identities', 'projects', 'portfolios',
        'project_memberships', 'project_repositories', 'role_grants',
        'event_sources', 'source_acl_entries', 'beads_databases', 'work_refs',
        'execution_nodes', 'execution_cells', 'execution_rigs', 'agent_profiles'
    ]
    LOOP
        EXECUTE format('SELECT count(*) FROM %I', t) INTO n;
        IF n <> 0 THEN
            bad := bad || format('  %s holds %s row(s)%s', t, n, chr(10));
        END IF;
    END LOOP;

    IF bad <> '' THEN
        RAISE EXCEPTION E'a fresh install is not empty:\n%\nSomething seeds records outside db/tenant_seeds.txt. A record about the real world belongs in the deployment''s database, not in a migration.', bad;
    END IF;
END
$$;

-- The permission model is schema and must survive the skip. If this is empty,
-- -fresh has skipped too much and nobody can be granted anything.
DO $$
DECLARE n bigint;
BEGIN
    SELECT count(*) INTO n FROM roles;
    IF n = 0 THEN
        RAISE EXCEPTION 'roles is empty; -fresh skipped a migration carrying the permission model';
    END IF;
END
$$;

-- The schema itself has to be there. A "clean" install that is clean because
-- nothing was created would pass every check above.
DO $$
DECLARE n bigint;
BEGIN
    SELECT count(*) INTO n FROM pg_tables WHERE schemaname = 'public';
    IF n < 35 THEN
        RAISE EXCEPTION 'only % tables in a fresh install; the schema did not apply', n;
    END IF;
END
$$;

SELECT 'a fresh install contains nobody, and the schema and permission model are present' AS result;
