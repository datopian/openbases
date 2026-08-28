-- Personal API tokens: visibility, revocation, and the constraints that are not
-- allowed to be only a Go validation (wg-p4h.2).
--
-- Two things are asserted here that unit tests cannot reach.
--
-- The RLS policy. A token is a credential, so one person seeing another's token
-- rows is a real finding rather than an information-disclosure footnote. Run as
-- workgraph_app, because as postgres the policies are never evaluated and every
-- assertion below would pass whether they were right, wrong, or absent.
--
-- The CHECK constraints. wg-p4h.2's whole argument for putting the ungrantable
-- scopes and the bounded lifetime in the schema is that a Go validation is one
-- forgotten call site away from being absent. That argument is only worth
-- anything if the constraints actually bite, which is what the owner-role
-- section at the end proves.

BEGIN;

-- ---------------------------------------------------------------------------
-- Seed
-- ---------------------------------------------------------------------------

INSERT INTO organisations (id, slug, name)
VALUES ('00000000-0000-0000-0000-0000000004a1', 'test-tokens', 'Test org: api_tokens');

INSERT INTO users (id, organisation_id, display_name, primary_email, status) VALUES
  ('00000000-0000-0000-0000-0000000004b1', '00000000-0000-0000-0000-0000000004a1', 'Ada',  'ada@example.com',  'active'),
  ('00000000-0000-0000-0000-0000000004b2', '00000000-0000-0000-0000-0000000004a1', 'Grace','grace@example.com','active'),
  ('00000000-0000-0000-0000-0000000004b3', '00000000-0000-0000-0000-0000000004a1', 'Gone', 'gone@example.com', 'suspended');

-- Digests are arbitrary 32-byte values. No plaintext token exists anywhere in
-- this file, deliberately: secret_scan.sh scans history as well as the working
-- tree, so a token-shaped literal here would fail CI permanently (wg-cjp).
INSERT INTO api_tokens (id, user_id, label, token_sha256, scopes, created_at, expires_at) VALUES
  ('00000000-0000-0000-0000-0000000004c1', '00000000-0000-0000-0000-0000000004b1',
   'ada laptop',      sha256('ada-live'::bytea),      ARRAY['work.create']::text[], now(),                    now() + interval '30 days'),
  -- Expired, and it has to be BACKDATED rather than have its expiry pushed
  -- into the past. api_tokens_expiry_after_creation applies to UPDATE as well
  -- as INSERT, so there is no way to represent a token that expired before it
  -- was created — which is right, and means an expired fixture is one that was
  -- minted two days ago for one day.
  ('00000000-0000-0000-0000-0000000004c2', '00000000-0000-0000-0000-0000000004b1',
   'ada expired',     sha256('ada-expired'::bytea),   '{}'::text[],                now() - interval '2 days', now() - interval '1 day'),
  ('00000000-0000-0000-0000-0000000004c3', '00000000-0000-0000-0000-0000000004b2',
   'grace laptop',    sha256('grace-live'::bytea),    '{}'::text[],                now(),                    now() + interval '30 days'),
  ('00000000-0000-0000-0000-0000000004c4', '00000000-0000-0000-0000-0000000004b3',
   'suspended owner', sha256('gone-live'::bytea),     '{}'::text[],                now(),                    now() + interval '30 days');

-- ---------------------------------------------------------------------------
-- Visibility, as the application role
-- ---------------------------------------------------------------------------

SET LOCAL ROLE workgraph_app;
\ir assert_app_role.sql

-- Ada sees her own two and nothing of Grace's.
SET LOCAL "workgraph.user_id" = '00000000-0000-0000-0000-0000000004b1';

DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM api_tokens;
  IF n <> 2 THEN
    RAISE EXCEPTION 'Ada sees % token rows, expected exactly her own 2', n;
  END IF;

  SELECT count(*) INTO n FROM api_tokens
   WHERE user_id <> '00000000-0000-0000-0000-0000000004b1';
  IF n <> 0 THEN
    RAISE EXCEPTION 'Ada can see % token rows belonging to someone else', n;
  END IF;
END $$;

-- Revocation is scoped by the same policy: Ada cannot revoke Grace's token, and
-- the failure is silently zero rows rather than an error, which is why this
-- asserts the row is still live afterwards rather than trusting the UPDATE.
DO $$
DECLARE affected int;
BEGIN
  UPDATE api_tokens SET revoked_at = now()
   WHERE id = '00000000-0000-0000-0000-0000000004c3';
  GET DIAGNOSTICS affected = ROW_COUNT;
  IF affected <> 0 THEN
    RAISE EXCEPTION 'Ada revoked % of Grace''s tokens', affected;
  END IF;
END $$;

-- Ada revoking her own token works, and takes effect immediately.
UPDATE api_tokens SET revoked_at = now()
 WHERE id = '00000000-0000-0000-0000-0000000004c1';

RESET ROLE;

-- ---------------------------------------------------------------------------
-- Authentication, through the SECURITY DEFINER lookup
-- ---------------------------------------------------------------------------
--
-- This runs with NO user set, which is the real calling convention: the token is
-- how the user is established, so at lookup time current_app_user() is NULL and
-- api_tokens' own policy would deny. That the function still resolves is the
-- point of it being SECURITY DEFINER.

SET LOCAL "workgraph.user_id" = '';

DO $$
DECLARE n int;
BEGIN
  -- Just revoked, so it must not authenticate. This is the assertion behind
  -- "revocation takes effect on the next request".
  SELECT count(*) INTO n FROM system_api_token_authenticate(sha256('ada-live'::bytea));
  IF n <> 0 THEN
    RAISE EXCEPTION 'a revoked token still authenticates';
  END IF;

  SELECT count(*) INTO n FROM system_api_token_authenticate(sha256('ada-expired'::bytea));
  IF n <> 0 THEN
    RAISE EXCEPTION 'an expired token still authenticates';
  END IF;

  -- The owner is suspended. Offboarding must not have to remember tokens
  -- separately; the one it forgot would be the one that mattered.
  SELECT count(*) INTO n FROM system_api_token_authenticate(sha256('gone-live'::bytea));
  IF n <> 0 THEN
    RAISE EXCEPTION 'a suspended user''s token still authenticates';
  END IF;

  SELECT count(*) INTO n FROM system_api_token_authenticate(sha256('no-such-token'::bytea));
  IF n <> 0 THEN
    RAISE EXCEPTION 'an unknown digest authenticated';
  END IF;
END $$;

-- Grace's live token resolves to Grace, and to nobody else.
DO $$
DECLARE u uuid;
BEGIN
  SELECT user_id INTO u FROM system_api_token_authenticate(sha256('grace-live'::bytea));
  IF u IS DISTINCT FROM '00000000-0000-0000-0000-0000000004b2' THEN
    RAISE EXCEPTION 'a live token resolved to % rather than its owner', u;
  END IF;
END $$;

-- Recording use is coarsened, so a token in constant use is written once per
-- interval rather than once per request.
DO $$
DECLARE first_seen timestamptz; second_seen timestamptz;
BEGIN
  PERFORM system_api_token_record_use('00000000-0000-0000-0000-0000000004c3');
  SELECT last_used_at INTO first_seen FROM api_tokens
   WHERE id = '00000000-0000-0000-0000-0000000004c3';
  IF first_seen IS NULL THEN
    RAISE EXCEPTION 'first use was not recorded at all';
  END IF;

  PERFORM system_api_token_record_use('00000000-0000-0000-0000-0000000004c3');
  SELECT last_used_at INTO second_seen FROM api_tokens
   WHERE id = '00000000-0000-0000-0000-0000000004c3';
  IF second_seen <> first_seen THEN
    RAISE EXCEPTION 'a second use within the window wrote again; the coarsening does nothing';
  END IF;
END $$;

-- ---------------------------------------------------------------------------
-- The constraints, as the owner
-- ---------------------------------------------------------------------------
--
-- Owner-role on purpose: these assert CHECK behaviour, which applies to every
-- role including the owner, and RLS is not the property under test. The point
-- is that a caller who bypasses the Go validation entirely still cannot store
-- one of these rows.

DO $$
BEGIN
  -- approval.decide is the scope that would let an agent approve its own
  -- dispatch and defeat ADR-0009's digest binding.
  BEGIN
    INSERT INTO api_tokens (user_id, label, token_sha256, scopes, expires_at)
    VALUES ('00000000-0000-0000-0000-0000000004b1', 'self approver',
            sha256('reject-approval'::bytea), ARRAY['approval.decide']::text[],
            now() + interval '1 day');
    RAISE EXCEPTION 'a token carrying approval.decide was accepted by the database';
  EXCEPTION WHEN check_violation THEN NULL;
  END;

  BEGIN
    INSERT INTO api_tokens (user_id, label, token_sha256, scopes, expires_at)
    VALUES ('00000000-0000-0000-0000-0000000004b1', 'merger',
            sha256('reject-merge'::bytea), ARRAY['pull_request.merge']::text[],
            now() + interval '1 day');
    RAISE EXCEPTION 'a token carrying pull_request.merge was accepted by the database';
  EXCEPTION WHEN check_violation THEN NULL;
  END;

  -- An unexpiring credential for a person is how someone who left keeps access.
  BEGIN
    INSERT INTO api_tokens (user_id, label, token_sha256, scopes, expires_at)
    VALUES ('00000000-0000-0000-0000-0000000004b1', 'forever',
            sha256('reject-forever'::bytea), '{}'::text[], now() + interval '365 days');
    RAISE EXCEPTION 'a token lasting a year was accepted; the lifetime bound does nothing';
  EXCEPTION WHEN check_violation THEN NULL;
  END;

  BEGIN
    INSERT INTO api_tokens (user_id, label, token_sha256, scopes, expires_at)
    VALUES ('00000000-0000-0000-0000-0000000004b1', '',
            sha256('reject-unlabelled'::bytea), '{}'::text[], now() + interval '1 day');
    RAISE EXCEPTION 'an unlabelled token was accepted';
  EXCEPTION WHEN check_violation THEN NULL;
  END;

  -- The digest column is a digest, not a place to put the token.
  BEGIN
    INSERT INTO api_tokens (user_id, label, token_sha256, scopes, expires_at)
    VALUES ('00000000-0000-0000-0000-0000000004b1', 'short digest',
            '\x00'::bytea, '{}'::text[], now() + interval '1 day');
    RAISE EXCEPTION 'a digest that is not 32 bytes was accepted';
  EXCEPTION WHEN check_violation THEN NULL;
  END;

  -- Two tokens cannot share a digest.
  BEGIN
    INSERT INTO api_tokens (user_id, label, token_sha256, scopes, expires_at)
    VALUES ('00000000-0000-0000-0000-0000000004b2', 'collision',
            sha256('grace-live'::bytea), '{}'::text[], now() + interval '1 day');
    RAISE EXCEPTION 'two tokens were allowed to share a digest';
  EXCEPTION WHEN unique_violation THEN NULL;
  END;
END $$;

-- Deleting a user takes their tokens with them, so offboarding is one action.
DO $$
DECLARE n int;
BEGIN
  DELETE FROM users WHERE id = '00000000-0000-0000-0000-0000000004b3';
  SELECT count(*) INTO n FROM api_tokens
   WHERE user_id = '00000000-0000-0000-0000-0000000004b3';
  IF n <> 0 THEN
    RAISE EXCEPTION 'deleting a user left % of their tokens behind', n;
  END IF;
END $$;

ROLLBACK;
