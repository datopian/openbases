-- system_bead_outcome agrees with system_bead_detail, for every bead.
--
-- The outcome is derived in two places. system_bead_detail answers for one
-- bead and assembles pull requests, spend and a log tail with it, which a list
-- cannot afford per row -- so system_bead_outcome exists for the project page.
--
-- Rewriting the two-hundred-line detail function to call the small one was the
-- alternative, and it was not taken: retyping a long function to change a few
-- lines is exactly how an IS DISTINCT FROM guard was dropped out of the cell
-- registration earlier the same day. Duplication that a test pins is safer
-- than a rewrite that nothing checks.
--
-- So this is that pin. It compares the two for every bead in the database
-- rather than for a fixture, because the interesting cases are the ones real
-- data produces: a bead with two runs, a bead whose work is in a pull request,
-- a bead never dispatched.
--
-- Owner-only by design, recorded in scripts/check_rls_tests.py: both functions
-- are SECURITY DEFINER and the property is that they agree, not who may read
-- them.
\set ON_ERROR_STOP on

DO $$
DECLARE
  r record;
  disagreed integer := 0;
  checked integer := 0;
  admin uuid;
BEGIN
  -- Both functions refuse an unauthenticated caller, so one is set. An
  -- organisation-wide admin, because can_read_project lets those read
  -- everything and the point of this test is to compare EVERY bead: a narrower
  -- caller would compare a subset and call it exhaustive.
  SELECT g.user_id INTO admin
    FROM role_grants g
   WHERE g.project_id IS NULL AND g.role_name = 'organisation_admin'
     AND (g.expires_at IS NULL OR g.expires_at > now())
   LIMIT 1;
  IF admin IS NULL THEN
      RAISE EXCEPTION 'no organisation admin to read as, so nothing could be compared';
  END IF;
  PERFORM set_config('workgraph.user_id', admin::text, true);
  FOR r IN SELECT bead_id FROM work_refs ORDER BY bead_id
  LOOP
    checked := checked + 1;
    IF (SELECT system_bead_outcome(r.bead_id))
       IS DISTINCT FROM (SELECT system_bead_detail(r.bead_id)->>'outcome') THEN
      disagreed := disagreed + 1;
      RAISE WARNING 'bead % : outcome says %, detail says %',
        r.bead_id,
        quote_nullable((SELECT system_bead_outcome(r.bead_id))),
        quote_nullable((SELECT system_bead_detail(r.bead_id)->>'outcome'));
    END IF;
  END LOOP;

  IF disagreed > 0 THEN
    RAISE EXCEPTION 'the two outcome derivations disagree on % of % beads; the '
      'project page and the bead page would tell a reader different things',
      disagreed, checked;
  END IF;

  -- A pin that pins nothing is worse than none: if work_refs is empty the loop
  -- never runs and this passes without comparing anything.
  IF checked = 0 THEN
    RAISE EXCEPTION 'no beads to compare, so this test asserted nothing';
  END IF;
  RAISE NOTICE 'compared % beads', checked;
END
$$;

SELECT 'the two outcome derivations agree' AS result;
