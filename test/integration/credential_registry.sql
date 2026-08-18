-- The credential registry: references only, and an honest overdue view.
--
-- These assert the properties the rotation tooling depends on. If the view lies
-- about what is overdue, the registry is worse than not having one — it is a
-- source of false confidence about which credentials are fresh.

BEGIN;

DO $$
DECLARE n integer; s text; d timestamptz;
BEGIN
  -- Every credential this system has must be registered. An unregistered
  -- credential is one nobody owns and nobody rotates.
  SELECT count(*) INTO n FROM credential_registry;
  IF n < 9 THEN
    RAISE EXCEPTION 'expected at least 9 registered credentials, found %', n;
  END IF;

  -- Every one has an accountable person and a documented way to revoke it. Both
  -- are wanted at the worst possible moment, so neither may be blank.
  SELECT count(*) INTO n FROM credential_registry
   WHERE owner_email = '' OR revocable_by = '' OR scope = '';
  IF n <> 0 THEN
    RAISE EXCEPTION '% credential(s) missing an owner, a scope or a revocation path', n;
  END IF;

  -- ---------------------------------------------------------------------
  -- Values must never be stored here
  -- ---------------------------------------------------------------------
  BEGIN
    INSERT INTO credential_registry
        (name, description, store, delivery, owner_email, scope, revocable_by)
    VALUES ('probe_pem', '-----BEGIN RSA PRIVATE KEY-----', 'sops', 'file',
            'x@datopian.com', 'probe', 'probe');
    RAISE EXCEPTION 'a PEM was accepted into the registry; the guard is not working';
  EXCEPTION WHEN raise_exception THEN
    IF SQLERRM LIKE '%references, not values%' THEN
      NULL;  -- refused, as intended
    ELSE
      RAISE;
    END IF;
  END;

  BEGIN
    INSERT INTO credential_registry
        (name, description, store, delivery, owner_email, scope, revocable_by)
    VALUES ('probe_tok', 'the value is cfat_0123456789abcdef0123456789abcdef', 'sops',
            'file', 'x@datopian.com', 'probe', 'probe');
    RAISE EXCEPTION 'a Cloudflare token was accepted into the registry';
  EXCEPTION WHEN raise_exception THEN
    IF SQLERRM LIKE '%references, not values%' THEN
      NULL;
    ELSE
      RAISE;
    END IF;
  END;

  -- ---------------------------------------------------------------------
  -- The overdue view has to be right, in every state
  -- ---------------------------------------------------------------------
  INSERT INTO credential_registry
      (name, description, store, delivery, owner_email, scope, revocable_by,
       rotated_at, rotate_every)
  VALUES ('probe_fresh', 'rotated yesterday, due in 90 days', 'sops', 'file',
          'x@datopian.com', 'probe', 'probe', now() - interval '1 day', '90 days');

  SELECT state INTO s FROM credential_rotation_due WHERE name = 'probe_fresh';
  IF s <> 'ok' THEN RAISE EXCEPTION 'a freshly rotated credential reads as %', s; END IF;

  UPDATE credential_registry SET rotated_at = now() - interval '100 days'
   WHERE name = 'probe_fresh';
  SELECT state, due_at INTO s, d FROM credential_rotation_due WHERE name = 'probe_fresh';
  IF s <> 'overdue' THEN RAISE EXCEPTION 'a credential 100 days past a 90 day cycle reads as %', s; END IF;
  IF d IS NULL THEN RAISE EXCEPTION 'an overdue credential has no due date'; END IF;

  -- 'never rotated' must be reachable WITH an interval set. An earlier version
  -- of the schema forbade that combination, which made the most urgent state in
  -- the table unrepresentable.
  UPDATE credential_registry SET rotated_at = NULL WHERE name = 'probe_fresh';
  SELECT state INTO s FROM credential_rotation_due WHERE name = 'probe_fresh';
  IF s <> 'never rotated' THEN
    RAISE EXCEPTION 'an unrotated credential with a rotation interval reads as %', s;
  END IF;

  -- Expiry outranks the rotation cycle: an expired credential is already
  -- broken, not merely due.
  UPDATE credential_registry
     SET rotated_at = now(), expires_at = now() - interval '1 hour'
   WHERE name = 'probe_fresh';
  SELECT state INTO s FROM credential_rotation_due WHERE name = 'probe_fresh';
  IF s <> 'expired' THEN RAISE EXCEPTION 'an expired credential reads as %', s; END IF;

  -- ---------------------------------------------------------------------
  -- Recording a rotation
  -- ---------------------------------------------------------------------
  IF NOT system_record_rotation('probe_fresh', now()) THEN
    RAISE EXCEPTION 'recording a rotation for a registered credential returned false';
  END IF;

  -- And an unregistered one must report false rather than silently doing
  -- nothing, so the script can refuse.
  IF system_record_rotation('definitely_not_registered', now()) THEN
    RAISE EXCEPTION 'recording a rotation for an unregistered credential returned true';
  END IF;

  RAISE NOTICE 'credential registry OK (% registered)', (SELECT count(*) FROM credential_registry);
END $$;

ROLLBACK;
