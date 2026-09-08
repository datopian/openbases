# An example environment

Copy this directory, rename it, and fill in the two `.example` files. It is a
copy of `envs/staging` with our values removed — the alternative is reading
somebody else's environment and guessing which parts are theirs, which is how a
deployment ends up half-configured with our hostnames in it.

```bash
cp -r infra/tofu/envs/example infra/tofu/envs/mine
cd infra/tofu/envs/mine
cp terraform.tfvars.example terraform.tfvars
cp backend.hcl.example backend.hcl
# fill both in, then
tofu init -backend-config=backend.hcl
tofu plan
```

There is no `terraform.tfvars` here on purpose, so `tofu apply` in this
directory refuses until someone has made the file deliberately.
`scripts/check_infra.py` fails if one appears, if a variable is declared without
being mentioned in the example, or if one of our values reappears in it.

## What you need before starting

| | Why |
|---|---|
| A Cloudflare account and a DNS zone | Every hostname, Access, and the tunnels. Use a zone you can grant DNS Write on — not necessarily your company domain, which usually carries the mail records. |
| An R2 bucket for OpenTofu state | State is the mapping between this config and the live infrastructure. Turn on versioning. |
| A Hetzner project and an SSH key | The two nodes. |
| A GitHub App | So agents can open pull requests. Its id and private key are deployment secrets, not tofu variables. |
| Your own SOPS/age key | `infra/secrets/*.enc.yaml` are encrypted to Datopian's keys and are not usable by anyone else. Create your own and re-encrypt from the templates. |

**Google Workspace ingestion is optional and off by default.** Leave
`google_project_id` empty unless you want Drive and Meet ingested: it is the
largest external setup here — a GCP project, domain-wide delegation, Pub/Sub —
and nothing else depends on it. Empty means the `google-events` module creates
nothing, the workspace timer is never enabled, and the monitor reports
ingestion as off rather than carrying a failing check you cannot clear.

## Then the deployment itself

The nodes are configured by Ansible, not by OpenTofu. Copy
`infra/ansible/inventory/example.yml`, change the four hostnames to the ones
this environment created, and see `infra/ansible/group_vars/example.yml.disabled`
for the settings whose committed values are ours — the execution cells in
particular, which are named for the clients they isolate.

Then, once the database exists:

```bash
workgraph-migrate -fresh    # schema and the permission model, nobody in it
wg-init -org ...            # your organisation and first administrator
```

`docs/install/deployable-by-others.md` has the full picture, including what is
still missing: nobody has yet followed this end to end on a clean host, which is
stage 4.

## One company per deployment

The schema has an `organisations` table, but cells, rigs, bead prefixes and the
Access applications all assume a single tenant. `wg-init` refuses to create a
second organisation for that reason. Multi-tenancy is not supported and is not
a small change.
