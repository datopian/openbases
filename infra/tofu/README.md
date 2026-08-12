# infra/tofu

OpenTofu configuration for every Workgraph environment. Nothing persistent is created by hand
(ADR-0015).

```
modules/
  environment/     one complete environment: network, firewall, nodes, tunnel, Access, DNS, R2
  google-events/   the minimal Google Cloud Pub/Sub delivery fabric for Workspace Events
envs/
  staging/         staging root module
  production/      production root module
```

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

1. `scripts/bootstrap_state_bucket.sh` — creates the R2 state bucket via the Cloudflare API.
   Idempotent, committed, so the step is represented in Git rather than remembered.
2. Mint R2 S3 credentials **scoped to that bucket**, not account-wide.
3. `tofu init -backend-config=...` and migrate state.
4. `tofu plan` → human approval → `tofu apply`.

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
