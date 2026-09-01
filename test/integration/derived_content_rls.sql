-- Every table holding source-derived content has a read policy.
--
-- This exists because the same gap was found three separate times in one day,
-- each by somebody happening to look: event_receipts, then knowledge_reviews
-- with source_acl_entries and source_snapshots, then human_corrections and
-- knowledge_record_sources. Each was created with row-level security in mind
-- somewhere else in the same file and simply not given a policy.
--
-- Asking the database rather than reading the migrations, because the question
-- is about the schema that exists rather than the one the files describe. A
-- table added tomorrow with a foreign key to a source is caught without anybody
-- remembering to add it here.
--
-- FORCE matters as much as ENABLE. Without it the table owner bypasses the
-- policy, and the owner is who the application connects as in several tools.

BEGIN;

-- The check as a function, so it can be run against the real schema AND against
-- a deliberately broken one. A guard nobody has watched fail is a guard nobody
-- knows works; this file proves both directions before it claims either.
CREATE FUNCTION pg_temp.check_derived_rls() RETURNS text
LANGUAGE plpgsql AS $fn$
DECLARE
  r record;
  problems text := '';
  n integer := 0;
BEGIN
  FOR r IN
    SELECT c.relname AS table_name,
           c.relrowsecurity AS rls_enabled,
           c.relforcerowsecurity AS rls_forced,
           (SELECT count(*) FROM pg_policies p
             WHERE p.schemaname = 'public' AND p.tablename = c.relname
               AND p.cmd IN ('SELECT', 'ALL')) AS read_policies
      FROM pg_class c
      JOIN pg_namespace ns ON ns.oid = c.relnamespace
     WHERE ns.nspname = 'public'
       AND c.relkind = 'r'
       -- Anything with a foreign key into the source or candidate tables holds
       -- something derived from a source.
       AND EXISTS (
         SELECT 1 FROM pg_constraint fk
          WHERE fk.conrelid = c.oid AND fk.contype = 'f'
            AND fk.confrelid IN ('knowledge_sources'::regclass,
                                 'knowledge_candidates'::regclass,
                                 'event_sources'::regclass))
       -- Declared exceptions. Each needs a reason, because adding a name here
       -- is a decision that a table's contents are safe for any authenticated
       -- caller to read.
       --
       --   event_subscriptions  operational state -- Google's subscription id,
       --                        expiry and state. No source content, and the
       --                        only reader is the reconciler through its
       --                        SECURITY DEFINER system path.
       --
       --   evaluation_cases     golden, regression and safety fixtures, which
       --                        0003 requires to be synthetic or redacted: "a
       --                        real transcript is never committed, so a case
       --                        referencing one points at its source and
       --                        fetches the encrypted snapshot with the
       --                        caller's permissions". The row carries a
       --                        source id, not source content.
       AND c.relname NOT IN ('event_subscriptions', 'evaluation_cases')
     ORDER BY c.relname
  LOOP
    n := n + 1;
    IF NOT r.rls_enabled THEN
      problems := problems || format(E'\n  %s: row-level security is not enabled', r.table_name);
    ELSIF NOT r.rls_forced THEN
      problems := problems || format(
        E'\n  %s: row-level security is enabled but not FORCED, so the owner bypasses it',
        r.table_name);
    ELSIF r.read_policies = 0 THEN
      problems := problems || format(
        E'\n  %s: forced with no SELECT policy, so it is readable by nobody and writable only '
        'through a definer function -- if that is intended, declare it as an exception with the reason',
        r.table_name);
    END IF;
  END LOOP;

  IF n = 0 THEN
    RAISE EXCEPTION 'no source-derived tables were found at all; this check is not looking at anything';
  END IF;
  IF problems <> '' THEN
    RETURN problems;
  END IF;
  RETURN format('ok: %s table(s) checked', n);
END
$fn$;

DO $$
DECLARE result text;
BEGIN
  -- 1. The real schema passes.
  result := pg_temp.check_derived_rls();
  IF result NOT LIKE 'ok:%' THEN
    RAISE EXCEPTION 'the live schema fails the check:%', result;
  END IF;
  RAISE NOTICE 'derived-content RLS, real schema: %', result;

  -- 2. Turning RLS off on a protected table must be caught. Without this the
  --    check could be looking at nothing and would still report success --
  --    which is how a guard becomes a decoration.
  ALTER TABLE human_corrections DISABLE ROW LEVEL SECURITY;
  result := pg_temp.check_derived_rls();
  IF result NOT LIKE '%human_corrections: row-level security is not enabled%' THEN
    RAISE EXCEPTION 'disabling RLS on human_corrections was not caught; got: %', result;
  END IF;
  ALTER TABLE human_corrections ENABLE ROW LEVEL SECURITY;

  -- 3. Enabled but NOT forced is the one that looks protected in any listing
  --    showing only relrowsecurity, so it gets its own case.
  ALTER TABLE human_corrections NO FORCE ROW LEVEL SECURITY;
  result := pg_temp.check_derived_rls();
  IF result NOT LIKE '%not FORCED%' THEN
    RAISE EXCEPTION 'an enabled-but-unforced table was not caught; got: %', result;
  END IF;
  ALTER TABLE human_corrections FORCE ROW LEVEL SECURITY;

  -- 4. Forced with no read policy: writable through a definer path, readable by
  --    nobody. Legitimate for some tables, which is why it must be DECLARED
  --    rather than discovered.
  DROP POLICY human_corrections_read ON human_corrections;
  result := pg_temp.check_derived_rls();
  IF result NOT LIKE '%no SELECT policy%' THEN
    RAISE EXCEPTION 'a table with no read policy was not caught; got: %', result;
  END IF;

  RAISE NOTICE 'derived-content RLS: all three failure shapes are caught';
END $$;

ROLLBACK;
