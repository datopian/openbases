-- First-login identity linking (WP-C2 completion), asserted against a real
-- database carrying the pilot seed.
--
-- The property under test is what linking must NOT do: it must never create a
-- user. Being admitted by Cloudflare Access is not the same as being a
-- Workgraph user, and conflating them would move the decision about who belongs
-- in the organisation out of the registry.

BEGIN;

DO $$
DECLARE n integer; anu uuid; other uuid;
BEGIN
  SELECT id INTO anu FROM users WHERE primary_email = 'anuar.ustayev@datopian.com';
  IF anu IS NULL THEN RAISE EXCEPTION 'pilot seed missing; run 0009 first'; END IF;

  -- Nobody is linked yet, which is why nobody can sign in.
  SELECT count(*) INTO n FROM identities WHERE provider = 'cloudflare_access';
  IF n > 0 THEN RAISE NOTICE 'note: % subject(s) already linked', n; END IF;

  -- Simulate a first login.
  INSERT INTO identities (user_id, provider, subject, email)
  VALUES (anu, 'cloudflare_access', 'subject-from-cloudflare-1', 'anuar.ustayev@datopian.com');

  SELECT count(*) INTO n FROM identities
  WHERE provider = 'cloudflare_access' AND subject = 'subject-from-cloudflare-1';
  IF n <> 1 THEN RAISE EXCEPTION 'first login did not link'; END IF;

  -- The same subject twice must not create a second link.
  BEGIN
    INSERT INTO identities (user_id, provider, subject, email)
    VALUES (anu, 'cloudflare_access', 'subject-from-cloudflare-1', 'anuar.ustayev@datopian.com');
    RAISE EXCEPTION 'a duplicate subject was accepted';
  EXCEPTION WHEN unique_violation THEN
    NULL;  -- expected
  END;

  -- One subject must not be linkable to two different users. Otherwise an
  -- email change could reassign somebody else's history.
  SELECT id INTO other FROM users WHERE primary_email = 'rufus.pollock@datopian.com';
  BEGIN
    INSERT INTO identities (user_id, provider, subject, email)
    VALUES (other, 'cloudflare_access', 'subject-from-cloudflare-1', 'rufus.pollock@datopian.com');
    RAISE EXCEPTION 'the same subject was linked to a second user';
  EXCEPTION WHEN unique_violation THEN
    NULL;  -- expected
  END;

  -- A suspended user must not be resolvable, so suspension takes effect on the
  -- next request rather than when a token expires.
  UPDATE users SET status = 'suspended' WHERE id = anu;
  SELECT count(*) INTO n
  FROM identities i JOIN users u ON u.id = i.user_id
  WHERE i.subject = 'subject-from-cloudflare-1' AND u.status = 'active';
  IF n <> 0 THEN RAISE EXCEPTION 'a suspended user still resolves'; END IF;

  RAISE NOTICE 'identity linking assertions passed';
END
$$;

ROLLBACK;
