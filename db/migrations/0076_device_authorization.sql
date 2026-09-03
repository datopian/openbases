-- The device authorization grant, so an agent with no way to hold a secret can
-- still get a credential (wg-8la, RFC 8628).
--
-- The problem it solves: minting a token requires an interactive Cloudflare
-- Access session, deliberately -- a token that could mint its own successor
-- makes revocation meaningless. That rule is right and it left sandboxed agents
-- with nowhere to go. Claude Cowork cannot be given an environment variable and
-- resets between tasks, so the only paths left were "install a private Go
-- module without GitHub access" and "ask the human to paste a credential into
-- a chat transcript". Both are wrong; the second is worse.
--
-- The flow keeps the rule and removes the dead end. The agent asks for a code,
-- a PERSON approves it in a browser where the Access session already lives, and
-- the agent polls until a token appears. The human act stays human; only the
-- typing moves.
--
-- Three properties this schema is shaped around:
--
--   the device_code is the agent's secret and is stored as a digest, exactly
--   like api_tokens.token_sha256, so a database read cannot impersonate a
--   pending client;
--
--   the user_code is short because a person types it, which makes it guessable
--   by comparison -- so it lives for ten minutes, is consumed on use, and
--   counts its own failed lookups;
--
--   approval names the approver, so the minted token is attributed to them and
--   not to "automation". That is the whole reason a service token was the wrong
--   answer here.
BEGIN;

CREATE TABLE device_authorizations (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- The agent's secret, never stored in the clear. Same shape and same
    -- reasoning as api_tokens.token_sha256: bytea because a digest is bytes,
    -- and UNIQUE so a collision is a constraint violation rather than two
    -- clients sharing a pending grant.
    device_code_sha256 bytea NOT NULL UNIQUE CHECK (length(device_code_sha256) = 32),

    -- What the person types. Stored in the clear on purpose: it is displayed
    -- back to them on the approval page, and a digest cannot be shown. Its
    -- protection is that it is short-lived, single-use and rate-limited, not
    -- that it is secret at rest.
    --
    -- Uppercase with a reduced alphabet -- no I, O, 0 or 1 -- because this is
    -- read aloud and typed by hand, and a code that turns into a support
    -- question is a code nobody uses.
    user_code    text NOT NULL UNIQUE CHECK (user_code ~ '^[A-HJ-NP-Z2-9]{4}-[A-HJ-NP-Z2-9]{4}$'),

    -- What the resulting token will be allowed to do, chosen by the CLIENT and
    -- shown to the approver. A client that asks for more than it needs is
    -- visible at the moment of approval, which is the only moment anybody is
    -- paying attention.
    scopes       text[] NOT NULL DEFAULT '{}',

    -- What the client calls itself, shown on the approval page. Not trusted --
    -- anybody can claim to be anything -- but a person deciding whether to
    -- approve deserves to see what asked.
    client_label text NOT NULL CHECK (length(trim(client_label)) > 0),

    -- Who approved it, and when. NULL until somebody does.
    approved_by  uuid REFERENCES users(id) ON DELETE CASCADE,
    approved_at  timestamptz,

    -- The token this grant produced. Set when the client collects it, which is
    -- what makes the grant single-use: a second poll finds it already set and
    -- is refused rather than minting again.
    token_id     uuid REFERENCES api_tokens(id) ON DELETE SET NULL,
    redeemed_at  timestamptz,

    -- Failed user_code lookups against this row. A short code typed by a human
    -- is the weakest thing here, so it is counted and capped.
    failed_lookups integer NOT NULL DEFAULT 0,

    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,

    -- A grant cannot be redeemed without being approved, and cannot be
    -- approved without an approver. Both are enforced here rather than only in
    -- the functions, because a row that violates either is not a state the
    -- flow has -- it is a bug that has already happened.
    CONSTRAINT approved_names_approver
        CHECK (num_nonnulls(approved_by, approved_at) IN (0, 2)),
    CONSTRAINT redeemed_was_approved
        CHECK (token_id IS NULL OR approved_by IS NOT NULL),
    CONSTRAINT redeemed_names_when
        CHECK (num_nonnulls(token_id, redeemed_at) IN (0, 2))
);

-- Pending grants only. The table is swept, so this stays small.
CREATE INDEX device_authorizations_pending ON device_authorizations (expires_at)
    WHERE token_id IS NULL;

-- Nobody reads this table through the API, and row-level security is how that
-- is guaranteed rather than assumed. Every path below is a SECURITY DEFINER
-- function; there is no policy, so a direct select returns nothing even to the
-- application role.
ALTER TABLE device_authorizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE device_authorizations FORCE ROW LEVEL SECURITY;

COMMIT;
