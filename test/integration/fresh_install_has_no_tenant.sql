-- A fresh install contains nobody, and still contains everything else.
--
-- Run against a database created with `workgraph-migrate -fresh`, which records
-- the migrations in db/tenant_seeds.txt as applied without running them.
--
-- This is the test that keeps the repository honest about being open source.
-- Without it the next seeding migration reintroduces Datopian into every
-- deployment, and the first to notice is somebody else installing it.
--
-- Runs as the owner, deliberately, and needs no entry in
-- scripts/check_rls_tests.py: it counts through dynamic SQL, so that checker
-- sees no table name here. Owner is also the RIGHT lens -- RLS is irrelevant to
-- proving a table is empty, and dropping to workgraph_app would weaken the
-- test, because a policy that hid rows would make an install that still
-- contains Datopian look clean.
\set ON_ERROR_STOP on

-- Deny by default: EVERY table must be empty except the three that carry the
-- schema's own reference data.
--
-- The first version listed the sixteen tables it expected to be empty, and
-- that list was incomplete: credential_registry was missing, so ten Datopian
-- addresses shipped in an install this test called clean. Enumerating what may
-- be populated is the only version that also covers the table somebody adds
-- next year.
DO $$
DECLARE
    t   record;
    n   bigint;
    bad text := '';
BEGIN
    FOR t IN
        SELECT c.relname
          FROM pg_class c
          JOIN pg_namespace ns ON ns.oid = c.relnamespace
         WHERE ns.nspname = 'public'
           AND c.relkind = 'r'
           -- roles and role_permissions are the permission model, seeded by
           -- 0001_core.sql as schema. schema_migrations is this tool's own
           -- bookkeeping.
           AND c.relname NOT IN ('roles', 'role_permissions', 'schema_migrations')
         ORDER BY c.relname
    LOOP
        EXECUTE format('SELECT count(*) FROM %I', t.relname) INTO n;
        IF n <> 0 THEN
            bad := bad || format('  %s holds %s row(s)%s', t.relname, n, chr(10));
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
    SELECT count(*) INTO n FROM role_permissions;
    IF n = 0 THEN
        RAISE EXCEPTION 'role_permissions is empty; every role would grant nothing';
    END IF;
END
$$;

-- Skipping a migration must not remove FUNCTIONS, and this is the check that
-- was missing.
--
-- 0027_execution_registry.sql defines four registration functions and seeds
-- nothing, but it was in the manifest, so -fresh skipped it and every fresh
-- install lacked them. 0052_register_source.sql the same. Both installs came up
-- reporting themselves clean, because nothing asked whether a function existed
-- — every assertion above was about rows.
DO $$
DECLARE
    want text;
    missing text := '';
BEGIN
    FOREACH want IN ARRAY ARRAY[
        -- 0027: a deployment cannot register a node or a cell without these,
        -- which means it cannot run an agent at all.
        'system_register_execution_node',
        'system_register_execution_cell',
        'system_attach_project_to_cell',
        'system_record_usage',
        -- 0052: Workspace source registration and the receipt path.
        'system_register_source',
        'system_record_source_acl',
        'system_pending_receipts',
        'system_mark_receipt_processed',
        -- 0091: the install cannot create its first administrator without it.
        'system_bootstrap_organisation'
    ]
    LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_proc WHERE proname = want) THEN
            missing := missing || '  ' || want || chr(10);
        END IF;
    END LOOP;

    IF missing <> '' THEN
        RAISE EXCEPTION E'a fresh install is missing functions:\n%\nA migration in db/tenant_seeds.txt carries schema. -fresh skips it, so the schema never arrives.', missing;
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

SELECT 'a fresh install contains nobody, and has the schema, the permission model and its functions' AS result;
