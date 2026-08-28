-- Personal API tokens: a credential a person can give a tool (wg-p4h.2).
--
-- ADR-0025 decided the shape and wg-p4h.1 proved the half that mattered: a
-- non-Access credential can carry an application user through the stack with
-- row-level security behaving identically, because provenance is not an input a
-- policy has. authz.WithUser takes a user id and sets workgraph.user_id;
-- current_app_user() reads that setting. Nothing downstream can tell how the
-- caller authenticated, which is what makes this table safe to add rather than a
-- second, parallel permission system.
--
-- So a token is not a new kind of principal. It resolves to the SAME user the
-- browser session resolves to, inherits that person's grants and visibility
-- unchanged, and may only ever be NARROWER than its owner.

BEGIN;

CREATE TABLE api_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Whose credential this is. ON DELETE CASCADE because removing a user must
    -- end their tools' access at the same instant it ends theirs — the same
    -- property internal/domain/resolver.go already gives the session path,
    -- where deleting the user record ends access now rather than when a token
    -- expires.
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- What it is for, in a human's words. Required, because the only question
    -- anyone asks of a six-month-old token is "what breaks if I revoke this",
    -- and an unlabelled row cannot answer it.
    label        text NOT NULL CHECK (length(trim(label)) > 0),

    -- The SHA-256 of the token, never the token.
    --
    -- bytea rather than text: a digest is bytes, and storing it hex-encoded
    -- invites a comparison against a differently-cased string that silently
    -- never matches. UNIQUE so a collision is a constraint violation rather
    -- than two users sharing a credential.
    token_sha256 bytea NOT NULL UNIQUE CHECK (length(token_sha256) = 32),

    -- The actions this token may perform, as a subset of its owner's grants.
    -- Empty means read-only: the authorisation layer intersects this with what
    -- the owner may do, and an empty intersection permits nothing beyond what
    -- row-level security already allows to be read.
    scopes       text[] NOT NULL DEFAULT '{}',

    created_at   timestamptz NOT NULL DEFAULT now(),

    -- Required, and bounded below by the CHECK. A credential for a person that
    -- never expires is how someone who left keeps portfolio access, and the
    -- absence of a NULL here is deliberate: there is no "does not expire"
    -- option to reach for under time pressure.
    expires_at   timestamptz NOT NULL,

    -- Not telemetry. It is how anyone answers "is this still needed" without
    -- guessing, and how an unused token gets noticed before it is abused.
    last_used_at timestamptz,

    -- Set once, never cleared. Revocation is a state a token cannot leave, so
    -- a mistaken revoke means minting a new token rather than un-revoking one
    -- whose secret may have been the reason it was revoked.
    revoked_at   timestamptz,

    -- Where it was minted from, for the audit trail.
    created_from text,

    CONSTRAINT api_tokens_expiry_after_creation
        CHECK (expires_at > created_at),

    -- A bounded maximum lifetime, enforced here rather than in Go.
    --
    -- In the application this is a constant somebody can raise in a hurry. As a
    -- constraint it is a migration, which is reviewable and shows up in a diff.
    CONSTRAINT api_tokens_bounded_lifetime
        CHECK (expires_at <= created_at + interval '90 days'),

    -- No protected action is grantable to a token. THIS IS THE IMPORTANT ONE.
    --
    -- authz.Action.Protected() marks six actions as normally requiring a durable
    -- human approval, and ADR-0009 binds an approval to a digest of the action
    -- so a decision cannot silently cover different parameters.
    --
    -- approval.decide is the sharpest of them. A personal token IS the person —
    -- that is what makes reads work — so an agent holding its owner's token
    -- could approve its own dispatch, which defeats the digest binding entirely.
    -- No scope value makes that safe, so it is not expressible.
    --
    -- Written as a constraint and not a validation in Go because a validation is
    -- one forgotten call site away from being absent, and this is the rule the
    -- whole approval model rests on. A future scope that belongs here is a
    -- migration and a review, which is the correct cost.
    CONSTRAINT api_tokens_no_protected_scopes
        CHECK (NOT (scopes && ARRAY[
            'approval.decide',
            'pull_request.merge',
            'deployment.execute',
            'secret.manage',
            'policy.manage',
            'marketing.publish',
            'knowledge.classification.downgrade'
        ]::text[]))
);

-- The authentication lookup is by digest and nothing else.
CREATE INDEX api_tokens_live_idx ON api_tokens (token_sha256)
    WHERE revoked_at IS NULL;

-- "What tokens do I have" and "revoke everything for this user".
CREATE INDEX api_tokens_by_user_idx ON api_tokens (user_id, created_at DESC);

ALTER TABLE api_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_tokens FORCE ROW LEVEL SECURITY;

-- A person sees and manages their own tokens. Nobody else's, including an
-- organisation admin: a token is a credential, and listing someone else's
-- credentials is not an administrative need that this table has to serve.
-- Ending another person's access is done by removing their user record, which
-- cascades.
--
-- Split by command rather than written as one FOR ALL policy. test/integration/
-- invariants.sql refuses FOR ALL outright, and the reason is in its comment: a
-- permissive FOR ALL also covers SELECT, so it re-opens every read it was meant
-- to leave alone. The four policies below say the same thing here, because the
-- predicate is identical for every command — but that is a property of this
-- table today, not a rule, and the split is what keeps it from silently
-- becoming untrue.
CREATE POLICY api_tokens_read ON api_tokens
    FOR SELECT
    USING (user_id = current_app_user());

CREATE POLICY api_tokens_insert ON api_tokens
    FOR INSERT
    WITH CHECK (user_id = current_app_user());

-- Revocation is an UPDATE, and both halves are checked: USING decides which
-- rows are visible to update, WITH CHECK refuses reassigning a token to another
-- user on the way out.
CREATE POLICY api_tokens_update ON api_tokens
    FOR UPDATE
    USING (user_id = current_app_user())
    WITH CHECK (user_id = current_app_user());

-- Deleting a token is not how it is revoked — revocation sets revoked_at and
-- keeps the row, so "why did this stop working" stays answerable. The policy
-- exists so that a delete which does happen is still scoped to the owner.
CREATE POLICY api_tokens_delete ON api_tokens
    FOR DELETE
    USING (user_id = current_app_user());

COMMIT;
