-- Register the cost importer's Cloudflare token (wg-oku).
--
-- Every credential the platform holds is listed in credential_registry so that
-- "what would an attacker reach with this" and "when was it last rotated" have
-- answers that do not depend on somebody remembering. A credential that exists
-- and is not registered is one that never gets rotated.
--
-- It is deliberately NOT cloudflare_api_token, which is already registered above
-- with a much larger blast radius: that token edits DNS, R2 buckets, Tunnels and
-- Access policies. This one reads gateway logs. Reusing the broad token would
-- have put all of those powers on the control node, in a process whose entire
-- job is reading a log — and the registry is where that difference is recorded
-- rather than argued about later.

BEGIN;

INSERT INTO credential_registry
    (name, description, store, delivery, owner_email, scope, revocable_by, rotate_every)
VALUES
    ('cost_import_cf_token',
     'Read-only Cloudflare API token for wg-costimport. AI Gateway: Read on one account; nothing else.',
     'sops', 'systemd-credential', 'anuar.ustayev@datopian.com',
     'Reads the logs of every AI Gateway on the account, which is prompt and response metadata but not content. It cannot change anything.',
     'Cloudflare dashboard > Manage Account > API Tokens > Revoke',
     '180 days')
ON CONFLICT (name) DO NOTHING;

COMMIT;
