-- What happened to a bead, available for a LIST and not only for one bead.
--
-- The project page shows a table and a dependency graph, and neither could say
-- that a bead had already been dispatched and produced nothing. sa-iyu ran for
-- 5m45s, exited 0, delivered nothing and left its bead open -- and appeared in
-- the graph as startable work, indistinguishable from a bead nobody had touched.
--
-- The derivation already exists inside system_bead_detail, which answers for
-- ONE bead. A list cannot call that per row -- it assembles pull requests,
-- spend and a log tail -- so the outcome is extracted here.
--
-- system_bead_detail is deliberately NOT rewritten to call this. It is a
-- two-hundred-line function, and retyping one to change a few lines is how I
-- dropped an IS DISTINCT FROM guard out of the cell registration earlier today.
-- Instead the two are asserted to AGREE, for every bead, by
-- test/integration/a_bead_says_what_happened.sql. Duplication that a test
-- pins is safer than a rewrite that nothing checks -- and if they ever
-- disagree, CI says which bead and both answers.
BEGIN;

DROP FUNCTION IF EXISTS system_bead_outcome(text);

-- One word for what happened to a bead, and the same word system_bead_detail
-- produces.
--
-- The order of the branches is load-bearing and is copied from there:
--
--   a queued or running job is reported as itself, not as an outcome;
--   `landed` comes BEFORE the closed check, because an open bead whose work is
--   in a pull request is awaiting review rather than stuck -- reporting that as
--   `blocked` was actively wrong once, while the change sat in portaljs#1662;
--   `blocked` is the honest word for a run that exited 0 and left the bead
--   open, which is an agent saying it could not finish.
-- Requires an authenticated caller, like system_bead_detail.
--
-- Not because the row set needs protecting here -- every caller reads it in a
-- SELECT list over rows an authorised query already chose -- but because the
-- two functions answer the same question and one of them refusing an
-- unauthenticated caller while the other answers is the kind of difference
-- that becomes a way in later. Found by the agreement test failing: detail
-- raised "a bead detail needs an authenticated caller" and this one answered.
CREATE OR REPLACE FUNCTION system_bead_outcome(p_bead text)
RETURNS text
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_run    text;
    v_bead   text;
    v_landed boolean;
BEGIN
    -- Requires an authenticated caller, like system_bead_detail.
    --
    -- Not because the row set needs protecting here -- every caller reads it
    -- in a SELECT list over rows an authorised query already chose -- but
    -- because the two functions answer the same question, and one refusing an
    -- unauthenticated caller while the other answers is the kind of
    -- difference that becomes a way in later. Found by the agreement test:
    -- detail raised "a bead detail needs an authenticated caller" and this
    -- answered.
    IF current_app_user() IS NULL THEN
        RAISE EXCEPTION 'a bead outcome needs an authenticated caller';
    END IF;

    SELECT status INTO v_run
      FROM work_queue WHERE bead = p_bead ORDER BY created_at DESC LIMIT 1;
    SELECT status INTO v_bead
      FROM work_refs WHERE bead_id = p_bead LIMIT 1;
    SELECT EXISTS (SELECT 1 FROM bead_pull_requests WHERE bead = p_bead)
      INTO v_landed;

    RETURN CASE
        WHEN v_run IS NULL THEN 'never_dispatched'
        WHEN v_run IN ('queued', 'running') THEN v_run
        WHEN v_run = 'failed' THEN 'failed'
        WHEN v_bead IS DISTINCT FROM 'closed' AND v_landed THEN 'landed'
        WHEN v_bead IS DISTINCT FROM 'closed' THEN 'blocked'
        ELSE 'done'
    END;
END
$$;

REVOKE ALL ON FUNCTION system_bead_outcome(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_bead_outcome(text) TO workgraph_app;

COMMIT;
