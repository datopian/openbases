-- The device authorization grant, end to end (wg-8la, RFC 8628).
--
-- The flow exists because minting a token requires an interactive session --
-- deliberately, since a token that could mint its own successor makes
-- revocation meaningless -- and that left a sandboxed agent with nowhere to go.
-- It cannot be given an environment variable, cannot open a browser, and must
-- not be handed a credential through a chat transcript.
--
-- What is asserted here is the whole state machine, including every refusal:
-- pending before approval, single approval, single redemption, an unknown code
-- answering invalid_grant rather than an error that leaks, and an ungrantable
-- scope refused at START rather than after somebody has approved it.

BEGIN;

SET LOCAL ROLE workgraph_app;
DO $p$
DECLARE
  me uuid; exp timestamptz; st record; ok boolean; tok uuid; n integer; caught text;
  dc bytea := sha256('a-device-secret'::bytea);
BEGIN
  -- The approver is created here rather than borrowed. A test that depends on
  -- a seeded person asserts an org chart, and CI's database is not staging's.
  RESET ROLE;
  INSERT INTO users (organisation_id, display_name, primary_email)
  SELECT id, 'Device flow approver', 'test-device-approver@example.invalid'
    FROM organisations WHERE slug = 'datopian'
  RETURNING id INTO me;
  SET LOCAL ROLE workgraph_app;

  -- 1. the client starts, with no credential at all
  PERFORM set_config('workgraph.user_id', '', true);
  exp := system_device_start(dc, 'ABCD-2345', 'Claude Cowork', ARRAY['project.read','work.create']);
  IF exp IS NULL THEN RAISE EXCEPTION 'start returned nothing'; END IF;

  -- 2. polling before approval is pending, not an error
  SELECT * INTO st FROM system_device_poll(dc);
  IF st.state <> 'authorization_pending' THEN RAISE EXCEPTION 'poll said %', st.state; END IF;

  -- 3. the approval page can show what is being asked
  SELECT count(*) INTO n FROM system_device_lookup('abcd-2345');
  IF n <> 1 THEN RAISE EXCEPTION 'lookup found % rows', n; END IF;

  -- 4. a person approves
  ok := system_device_approve('ABCD-2345', me);
  IF NOT ok THEN RAISE EXCEPTION 'approve refused a valid code'; END IF;

  -- 5. now the poll carries the approver and the scopes
  SELECT * INTO st FROM system_device_poll(dc);
  IF st.state <> 'approved' OR st.approver <> me THEN
    RAISE EXCEPTION 'poll after approval: % %', st.state, st.approver;
  END IF;
  IF NOT (st.scopes @> ARRAY['work.create']) THEN RAISE EXCEPTION 'scopes lost'; END IF;

  -- 6. approving twice is refused: one grant, one approval
  IF system_device_approve('ABCD-2345', me) THEN
    RAISE EXCEPTION 'a grant was approved twice';
  END IF;

  -- 7. redeem binds a token and consumes the grant.
  --
  -- The identity is set to the approver first, because that is what Go does:
  -- tokens.Mint runs inside authz.WithUser(approver), so api_tokens' insert
  -- policy sees the owner writing their own credential. Without it the policy
  -- refuses -- which the first version of this probe discovered.
  PERFORM set_config('workgraph.user_id', me::text, true);
  INSERT INTO api_tokens (user_id, label, token_sha256, scopes, expires_at)
  VALUES (me, 'device probe', sha256('probe-token'::bytea), ARRAY['project.read'], now()+interval '8 hours')
  RETURNING id INTO tok;
  IF NOT system_device_redeem(dc, tok) THEN RAISE EXCEPTION 'redeem refused'; END IF;

  -- 8. redeeming twice is refused, and the grant is now invalid
  IF system_device_redeem(dc, tok) THEN RAISE EXCEPTION 'a grant was redeemed twice'; END IF;
  SELECT * INTO st FROM system_device_poll(dc);
  IF st.state <> 'invalid_grant' THEN RAISE EXCEPTION 'after redemption poll said %', st.state; END IF;

  -- 9. an unknown device code is invalid_grant, never an error that leaks
  SELECT * INTO st FROM system_device_poll(sha256('never-issued'::bytea));
  IF st.state <> 'invalid_grant' THEN RAISE EXCEPTION 'unknown code said %', st.state; END IF;

  -- 10. an ungrantable scope is refused at START, before a person is asked
  BEGIN
    PERFORM system_device_start(sha256('x'::bytea), 'BBBB-3333', 'sneaky',
                                ARRAY['knowledge.review']);
    caught := '(nothing raised)';
  EXCEPTION WHEN others THEN caught := sqlerrm;
  END;
  IF caught NOT LIKE '%cannot hold that action%' THEN
    RAISE EXCEPTION 'requesting knowledge.review reported: %', caught;
  END IF;

  -- 11. the audit trail names the approver
  SELECT count(*) INTO n FROM audit_log
   WHERE action='device.authorization.approve' AND actor_user_id=me;
  IF n < 1 THEN RAISE EXCEPTION 'the approval was not audited'; END IF;

  RAISE NOTICE 'device flow: all eleven assertions passed';
END $p$;

ROLLBACK;
