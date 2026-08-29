-- Per-token request rate and daily spend caps (wg-p4h.9).
--
-- ADR-0022 gates spend per bead, and that stops one bead running away. It does
-- nothing about a client creating four hundred beads, each perfectly within
-- budget. A person clicking a button is their own rate limit; a loop in
-- somebody's harness is not, and wg-p4h.3 made it possible for a program to hold
-- the credential.
--
-- The limits live on the token rather than the user on purpose. One
-- misconfigured tool should be revocable and throttleable without cutting its
-- owner off, and attributing the spend to the credential is what makes "which
-- of my tokens is burning money" answerable at all.

BEGIN;

ALTER TABLE api_tokens
    -- 60/minute: an interactive tool polling once a second with headroom, and
    -- far below what a retry loop reaches. Per token, so a badly-behaved client
    -- throttles itself and not its owner's browser session.
    ADD COLUMN rate_per_minute integer NOT NULL DEFAULT 60
        CHECK (rate_per_minute > 0 AND rate_per_minute <= 6000),

    -- $5/day. Chosen to be small enough that a runaway is noticed the same day
    -- and large enough for real work: a bead has cost between one and a few
    -- cents in practice, so this is hundreds of dispatches, not three.
    --
    -- Zero is permitted and means "may not spend at all", which is the right
    -- setting for a read-only token and is not the same as no limit. There is
    -- deliberately no way to express "unlimited": that is the setting somebody
    -- reaches for at 2am and never revisits.
    ADD COLUMN daily_spend_cents numeric(12,4) NOT NULL DEFAULT 500
        CHECK (daily_spend_cents >= 0);

-- Which token asked for a job, so its spend can be attributed back.
--
-- ON DELETE SET NULL rather than CASCADE: deleting a token must not delete the
-- history of work it queued. The job happened, it cost money, and that record
-- outlives the credential.
ALTER TABLE work_queue
    ADD COLUMN requested_by_token uuid REFERENCES api_tokens(id) ON DELETE SET NULL;

CREATE INDEX work_queue_by_token ON work_queue (requested_by_token, created_at DESC)
    WHERE requested_by_token IS NOT NULL;

-- What a token has spent today, in cents.
--
-- Joins usage_records to the queue by bead, which is the link 0029 established
-- when it made spend attributable to a bead at all. A plan job has no bead until
-- it produces some, so its own cost is counted through the beads it created only
-- once those are dispatched — stated because it means a plan job's spend lands
-- slightly after the job, not because it is lost.
--
-- SECURITY DEFINER for the same reason as the token lookup: this runs while
-- deciding whether to admit a request, before any user session is open.
CREATE OR REPLACE FUNCTION system_api_token_spent_today(p_token_id uuid)
RETURNS numeric
LANGUAGE sql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
    SELECT coalesce(sum(u.cost_cents), 0)::numeric
      FROM work_queue q
      JOIN usage_records u ON u.bead = q.bead
     WHERE q.requested_by_token = p_token_id
       AND q.bead IS NOT NULL
       -- Calendar day in UTC. A rolling window would be fairer and harder to
       -- explain; "it resets at midnight UTC" is a sentence an operator can act
       -- on at the moment they are refused.
       AND u.occurred_at >= date_trunc('day', now() AT TIME ZONE 'UTC');
$$;

-- The cap and the spend together, so a caller asks once.
CREATE OR REPLACE FUNCTION system_api_token_budget(p_token_id uuid)
RETURNS TABLE (
    cap_cents   numeric,
    spent_cents numeric,
    exceeded    boolean
)
LANGUAGE sql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
    SELECT t.daily_spend_cents,
           system_api_token_spent_today(t.id),
           system_api_token_spent_today(t.id) >= t.daily_spend_cents
      FROM api_tokens t
     WHERE t.id = p_token_id;
$$;

REVOKE ALL ON FUNCTION system_api_token_spent_today(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_api_token_budget(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_api_token_spent_today(uuid) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_api_token_budget(uuid) TO workgraph_app;

COMMIT;
