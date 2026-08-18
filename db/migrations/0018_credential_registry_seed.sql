-- Register the credentials this system actually has (WP-B3).
--
-- Nine real credentials, each with an owner, a store, a delivery mechanism, its
-- blast radius, and who can revoke it. Written down because "rotate everything"
-- is not actionable without a list, and because the revocation column is the one
-- anybody will want at the worst possible moment.
--
-- Rotation intervals are deliberately unequal. A credential that can spend money
-- or reach every repository is on a shorter cycle than one that names a Hetzner
-- project.

BEGIN;

INSERT INTO credential_registry
    (name, description, store, delivery, owner_email, scope, revocable_by, rotate_every)
VALUES
    ('cloudflare_api_token',
     'Deploy token: tunnels, Access apps and policies, R2 buckets, DNS on the openbases.com zone only.',
     'sops', 'environment', 'anuar.ustayev@datopian.com',
     'One Cloudflare account shared with ~50 Datopian production zones; DNS scoped to openbases.com so it cannot touch datopian.com.',
     'Cloudflare dashboard > Manage Account > API Tokens > Revoke',
     '90 days'),

    ('hcloud_token',
     'Hetzner Cloud API token for the workgraph project. Creates and destroys nodes.',
     'sops', 'environment', 'anuar.ustayev@datopian.com',
     'One Hetzner project. Can destroy both staging nodes.',
     'Hetzner Console > Security > API tokens',
     '180 days'),

    ('tofu_state_passphrase',
     'OpenTofu state encryption passphrase. State holds tunnel credentials and the IdP client secret.',
     'sops', 'environment', 'anuar.ustayev@datopian.com',
     'All Terraform state for both environments. Losing it makes state unreadable.',
     'Not revocable — rotation means re-encrypting state, and an offline copy must exist first.',
     NULL),

    ('db_app_password',
     'PostgreSQL workgraph_app role. Deliberately not the table owner, so row-level security applies.',
     'sops', 'systemd-credential', 'anuar.ustayev@datopian.com',
     'The control-plane database, through a role that RLS constrains.',
     'ALTER ROLE workgraph_app PASSWORD, then redeploy',
     '90 days'),

    ('github_webhook_secret',
     'HMAC secret for inbound GitHub webhooks. GitHub cannot pass Access, so this is the only check.',
     'sops', 'systemd-credential', 'anuar.ustayev@datopian.com',
     'Webhook authenticity only. It grants no read access.',
     'Rotate in the GitHub App settings and in SOPS together',
     '90 days'),

    ('github_app_private_key',
     'GitHub App private key. Mints installation tokens for every installed repository.',
     'sops', 'systemd-credential', 'anuar.ustayev@datopian.com',
     'Every repository the App is installed on, including restricted client repositories. Held on the control node only; an execution node must never see it.',
     'GitHub App settings > Private keys > delete the key',
     '90 days'),

    ('ai_gateway_token',
     'Cloudflare AI Gateway token used by execution cells. Account-scoped, so it reaches every gateway.',
     'sops', 'file', 'anuar.ustayev@datopian.com',
     'All three AI Gateways and therefore the whole inference budget. Cannot be scoped to one gateway (wg-4r2).',
     'Cloudflare dashboard > AI > AI Gateway > tokens',
     '90 days'),

    ('google_workspace_client_secret',
     'OAuth client secret for the Google Workspace identity provider used by Cloudflare Access.',
     'sops', 'api-only', 'anuar.ustayev@datopian.com',
     'The sign-in path for every human user.',
     'Google Cloud console > Credentials > reset the secret',
     '180 days'),

    ('r2_secret_access_key',
     'R2 S3 credential for the OpenTofu state backend and, once scoped, the backup buckets.',
     'sops', 'environment', 'anuar.ustayev@datopian.com',
     'Currently the tfstate bucket only; does not yet cover backups, evidence or audit (wg-ohk).',
     'Cloudflare dashboard > R2 > Manage API tokens',
     '180 days')
ON CONFLICT (name) DO NOTHING;

COMMIT;
