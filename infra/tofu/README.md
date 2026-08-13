# infra/tofu

OpenTofu configuration for every Workgraph environment. Nothing persistent is created by hand
(ADR-0015).

```
account/           account-level singletons: the Zero Trust organisation
modules/
  environment/     one complete environment: network, firewall, nodes, tunnel, Access, DNS, R2
  google-events/   the minimal Google Cloud Pub/Sub delivery fabric for Workspace Events
envs/
  staging/         staging root module
  production/      production root module
```

## account/

The Cloudflare Zero Trust organisation is a singleton per account, so it cannot live in the
environment module — that module is instantiated once per environment, and two instances would
fight over one organisation.

It is **imported**, not created: enabling Zero Trust in the dashboard creates it with a generated
team domain. `account/` then owns the settings that plan §8.1 requires:

| Setting | Value | Why |
|---|---|---|
| `auth_domain` | `datopian.cloudflareaccess.com` | Visible on every login screen and the JWT issuer the control API validates. The generated `icy-boat-89aa` reads like a phishing domain to a client. |
| `mfa_required_for_all_apps` | `true` | Enforced org-wide rather than per application, so a new Access application cannot omit it. |
| `deny_unmatched_requests` | **`false`** | Set to `true` on 2026-08-12 and it returned 403 error 1050 across live zones on this account. Reverted. See the warning below. |
| `auto_redirect_to_identity` | `false` | Keeps the login screen that shows which organisation is asking for identity — the check that makes a phishing domain noticeable. |

`auth_domain` is written as the **full** domain because the API stores and returns it that way.
Writing the bare label produces a diff on every subsequent plan.

### Adding the Google Workspace identity provider

Google Workspace is chosen over the generic "Google" integration because it restricts sign-in to one
Workspace domain **and** can read group membership. Groups are what WP-C2 maps onto Workgraph roles,
so taking the generic integration now would mean redoing it later.

Three of these steps are the ones people miss, and each fails in a way that does not name itself.

**In the Google Cloud console** — the same project WP-H1 needs for Pub/Sub can host this:

1. **APIs & Services → Enable APIs → Admin SDK API.** Without it, group membership never returns.
2. **Configure Consent Screen**, audience type **Internal**. This blocks ordinary `@gmail.com`
   accounts from even reaching the login step. Choosing External here is a real exposure.
3. **Create OAuth Client**, type *Web application*:
   - Authorized JavaScript origin: `https://datopian.cloudflareaccess.com`
   - Authorized redirect URI: `https://datopian.cloudflareaccess.com/cdn-cgi/access/callback`
4. Copy the **Client ID** and **Client secret**.

**In the Google Admin console** (not Cloud — a different console):

5. Tick **Trust internal apps**. It is **off by default and Access does not work without it.** This
   is the single most common failure, and the symptom is a group-fetch error that never mentions
   trust.

   The reliable route is the direct URL, because Google has renamed this area several times:

   ```
   https://admin.google.com/ac/owl/settings
   ```

   By menu: **admin.google.com → Security → Access and data control → API controls**, then the
   **Settings** link on that page (not the left nav). The control sits under an **Internal apps**
   heading and is labelled *"Trust internal apps"* or, in newer consoles,
   *"Trust internal, domain-owned apps"*.

   If the page is not there at all, the account is not a **super admin** — delegated admin roles do
   not see API controls. That is the usual reason it "cannot be found".

**Then here:**

```bash
# Client ID is not a credential; commit it.
#   infra/tofu/account/terraform.tfvars → google_workspace_client_id = "…"
# The secret is:
echo 'TF_VAR_google_workspace_client_secret=…' >> ~/.config/datopian-workgraph/credentials.env
tofu -chdir=infra/tofu/account plan
```

6. After the provider is created, Cloudflare generates a **one-time authorisation link** that a
   Google Workspace **administrator** must visit to grant group-read consent. Setup is not complete
   until someone opens it, and testing before that returns
   *"Failed to fetch group information from the identity provider"* — which reads like a
   misconfiguration but is just the unfinished step.

7. Verify in the dashboard under Identity providers → **Test**. It should return your identity
   *and* your group membership.

One incompatibility worth knowing: the Google Workspace integration is not supported if the Google
Workspace account is itself protected by Cloudflare Access.

Renaming the team domain invalidates enrolled devices, registered identity-provider callback URLs,
and saved bookmarks. It is cheap only before any Access application exists.

### ⚠️ This Cloudflare account is shared with Datopian production

Workgraph does not own this account. It carries roughly 50 zones — `datopian.com`, `portaljs.com`,
`datahub.io`, `viderum.com` and others — that have nothing to do with Workgraph.

Every setting in `account/` is therefore **production-affecting**, whatever its name suggests.
`deny_unmatched_requests = true` was applied on the reasoning that with zero Access applications it
could not affect anything; it returned 403 across live zones until reverted.

Rules that follow, two of which are now enforced rather than remembered:

- Prefer a per-application control over an account-level one whenever both exist.
- Apply account-level changes **one at a time**, so an effect can be attributed.
- Reason about *observed* behaviour, not about what a setting's name implies.
- **`scripts/check_infra.py` fails on any account-level setting not in
  `REVIEWED_ACCOUNT_ATTRS`.** Adding a name there is a claim that you have established what the
  setting does to traffic that is not ours. `deny_unmatched_requests` is pinned to `false` and the
  build fails if it changes — declared explicitly rather than omitted, so Terraform *reverts* a
  dashboard change instead of ignoring it.
- **Run `scripts/check_live_zones.sh` after every `account/` apply**, before calling it successful.
  It smoke-checks the shared production zones. A 403 there means revert first and diagnose after.

A separate Cloudflare account would make all of this unnecessary by making account-level settings
genuinely ours. It was weighed and deferred: a new account means a new team domain, which means a
new OAuth redirect URI, which means redoing the Google client and its admin consent. The guards
above cost nothing and address the failure that actually happened.

## Required environment

Credentials never live in this repository.

Non-secret configuration — account and zone identifiers, hostname, location, and the
Access allow-list — lives in each environment's committed `terraform.tfvars`. ADR-0015
requires persistent configuration to be in Git, or production cannot be rebuilt from code.
Those identifiers appear in every dashboard URL and grant nothing on their own.

Only credentials come from the environment:

```bash
set -a; . ~/.config/datopian-workgraph/credentials.env; set +a   # bootstrap only; WP-B3 replaces this
# HCLOUD_TOKEN and CLOUDFLARE_API_TOKEN are read directly by the providers.
```

`scripts/check_infra.py` fails the build if a committed `.tfvars` ever grows something
credential-shaped.

### Adding or removing a person

`access_allowed_emails` in the environment's `terraform.tfvars` is the join and leave path
(runbooks 2 and 3). An empty list creates no Access policy at all, so the application denies
everyone — the correct failure direction, and also the reason a fresh environment locks
everyone out until the list is populated.

## State encryption

Creating a Cloudflare Tunnel necessarily produces a credential, and that credential lands in state.
Plan §11.4 forbids plaintext secrets in Terraform state, so **state and plan files are encrypted at
rest**, enforced.

Encryption is configured through `TF_ENCRYPTION`, not an `encryption` block in HCL. A block
referencing `var.` cannot be evaluated statically, which breaks `tofu providers schema` and every
other command that runs before variables resolve — a mistake worth not repeating.

```bash
export TF_ENCRYPTION='
key_provider "pbkdf2" "main" {
  passphrase = "'"$TOFU_STATE_PASSPHRASE"'"
}
method "aes_gcm" "main" {
  keys = key_provider.pbkdf2.main
}
state {
  method   = method.aes_gcm.main
  enforced = true
}
plan {
  method   = method.aes_gcm.main
  enforced = true
}
'
```

The passphrase must be at least 16 characters. **Losing it means losing the ability to read state.**
It is held with the age recovery key, with an offline copy under Datopian leadership (plan §11.4).

## Bootstrap order

There is a genuine chicken-and-egg: remote state wants an R2 bucket, and the bucket is created by
OpenTofu. Resolved by creating the state bucket once with an idempotent committed script, then
migrating state into it.

1. `scripts/bootstrap_state_bucket.sh <env>` — creates the R2 state bucket via the Cloudflare API.
   Idempotent, committed, so the step is represented in Git rather than remembered. **Done:**
   `workgraph-tfstate-staging` and `workgraph-tfstate-production`, both WEUR.
2. Mint R2 S3 credentials **scoped to those buckets**, not account-wide. See below.
3. `scripts/migrate_state_to_r2.sh <module>` — init against `backend.hcl`, migrate, verify the
   state reads back, and confirm no drift.
4. `tofu plan` → human approval → `tofu apply` → `make live-zones`.

### Minting the R2 credentials

**This account holds 74 R2 buckets**, including client and product data — `ckan-city-of-malmo`,
`dx-birmingham-city-prod`, `datahub-cloud` and others. An account-wide R2 token would reach every
one of them, from a credential that only needs to write two state files. Scope it.

Dashboard → **R2 → API → Manage API tokens → Create API token**:

| Field | Value |
|---|---|
| Permission | **Object Read & Write** |
| Specify buckets | **Apply to specific buckets only** — `workgraph-tfstate-staging`, `workgraph-tfstate-production` |
| TTL | Leave open, or set one and add it to the rotation runbook |

Copy the **Access Key ID** and **Secret Access Key** into `R2_ACCESS_KEY_ID` and
`R2_SECRET_ACCESS_KEY`. The account ID in the S3 endpoint is already in each `backend.hcl`.

Creating this credential is `secret.create` under `policies/default.yaml` — a protected action, and
correctly a human one.

## Applying is a protected action

`infrastructure.apply` requires an approval bound to the exact plan artefact, and
`infrastructure.destroy` requires two approvers (`policies/default.yaml`). In practice:

```bash
tofu plan -out=tfplan          # produce the artefact
tofu show -json tfplan | jq '[.resource_changes[] | select(.change.actions | index("delete"))]'
tofu apply tfplan              # only after approval, and only this artefact
```

Plan files are encrypted too, so `tofu show` needs `TF_ENCRYPTION` exported in the
same shell. Without it you get *"the given plan file is encrypted and requires a valid
encryption configuration to decrypt"* — the encryption working, not a corrupt plan.

**Always check the delete list before approving.** Any planned deletion of a record this
configuration does not own is a bug, not something to approve through.

### Why `openbases.com` and not `datopian.com`

The deploy token needs `DNS Write` on whichever zone serves these hostnames. `datopian.com` carries
Datopian's Google Workspace MX records, so `DNS Write` there would let a leaked bootstrap credential
redirect company email, pass DNS-01 validation to obtain trusted TLS certificates for
`datopian.com`, and take over anything using DNS for domain verification.

`openbases.com` was dormant — no A, MX, or TXT records — so this configuration owns the whole zone
with no collateral, and the token cannot reach the primary domain at all.

## Deliberate design choices

- **The firewall has no inbound rules.** Hetzner firewalls are allow-lists, so this drops every
  inbound connection. The origin is reached only through Cloudflare Tunnel, which connects outbound.
- **`admin_ssh_cidrs` defaults to empty.** Setting it opens a public SSH port and shows up loudly in
  the plan diff. That visibility is the point.
- **Nodes carry `prevent_destroy`.** Rebuilding a node is a runbook, not a side effect of editing an
  image or server type.
- **An empty `access_allowed_emails` creates no Access policy**, so the application denies everyone.
  Failing closed beats inventing a placeholder identity.
- **Production splits control and execution nodes** across a spread placement group. Agent builds are
  noisy and must never starve or compromise the system of record.
