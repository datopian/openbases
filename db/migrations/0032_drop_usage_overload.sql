-- Remove the old system_record_usage signature (wg-qw1).
--
-- 0029 added `p_bead text DEFAULT NULL` to system_record_usage with
-- CREATE OR REPLACE. That does NOT replace the function: a different argument
-- count is a different function, so the thirteen-argument version from 0027 was
-- still there and every existing call became ambiguous —
--
--   ERROR: function system_record_usage(unknown, ..., timestamptz) is not unique
--   HINT:  Could not choose a best candidate function.
--
-- which is what the cost importer does on every row. The migration applied
-- cleanly and broke the caller, the same shape as the c.name/c.slug bug in 0026:
-- a green migration means the SQL parsed, not that anything still works.
--
-- Caught by CI rather than by a person, because test/integration ran a
-- thirteen-argument call. That test was written for a different reason and paid
-- for itself here.
--
-- Dropped explicitly rather than left to be shadowed. Two overloads where one
-- has a default is a trap that fires at the CALL site, far from the definition,
-- and leaving it in place would mean every future caller has to know.

BEGIN;

DROP FUNCTION IF EXISTS system_record_usage(
    text, text, text, text, bigint, bigint, numeric, boolean, boolean,
    text, text, text, timestamptz);

COMMIT;
