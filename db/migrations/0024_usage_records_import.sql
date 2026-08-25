-- Make usage_records able to hold what the AI Gateway actually reports (wg-2a0).
--
-- The table has existed since 0006 and has never held a row. Cost lives only in
-- Cloudflare's gateway logs, which rotate on a 10,000,000-entry DELETE_OLDEST
-- policy and cannot be joined to a project or a bead. So "cost is a first-class
-- object" is currently a schema with no data in it, and every cost question is
-- answered by a script hitting an API that will eventually forget.
--
-- Three changes, each forced by what the gateway returns.
--
-- PRECISION. cost_cents was an integer, and a typical request costs $0.000088 —
-- 0.0088 cents. Every row would round to zero and the total of a month's real
-- spend would be zero. That is worse than no column: a report that confidently
-- says nothing was spent. Changed to numeric, keeping the name, because the unit
-- is still cents and renaming would break nothing today but confuse every reader
-- of the original migration.
--
-- IDENTITY. Each log entry carries a ULID. Recording it makes the import
-- idempotent: re-running after a partial failure, or overlapping windows, cannot
-- double-count. Without it the only safe import is one that never retries, which
-- is not an import.
--
-- ATTRIBUTION. The gateway records which gateway served a request and, when the
-- caller sent cf-aig-metadata, which role, cell and rig it came from. Storing
-- those is what turns a bill into an answer to "what did this project cost".
-- They are nullable because most historical traffic is untagged — 98.3% of it,
-- measured — and a NULL that says "we do not know" is worth more than a default
-- that quietly attributes it to someone.

BEGIN;

ALTER TABLE usage_records
    -- numeric, not integer. See PRECISION above.
    ALTER COLUMN cost_cents TYPE numeric(16, 8),
    ALTER COLUMN cost_cents SET DEFAULT 0;

ALTER TABLE usage_records
    ADD COLUMN IF NOT EXISTS gateway     text,
    ADD COLUMN IF NOT EXISTS external_id text,
    ADD COLUMN IF NOT EXISTS cached      boolean,
    ADD COLUMN IF NOT EXISTS succeeded   boolean,
    ADD COLUMN IF NOT EXISTS role        text,
    ADD COLUMN IF NOT EXISTS cell        text,
    ADD COLUMN IF NOT EXISTS rig         text;

-- The idempotency key. Partial, because rows written by anything other than the
-- gateway import have no external id and must not collide with each other.
CREATE UNIQUE INDEX IF NOT EXISTS usage_records_external_id
    ON usage_records (external_id) WHERE external_id IS NOT NULL;

-- Answering "what did this cost, by model" over a month is the query this table
-- exists for, and without this it is a sequential scan of everything.
CREATE INDEX IF NOT EXISTS usage_records_time_model
    ON usage_records (occurred_at DESC, model);

COMMIT;
