# Letting an agent deploy to PortalJS Arc

An agent that has built something can now deploy it, once the cell holds an
Arc token. This is what sa-fj3 was missing: it built the portal, produced a
clean static export, verified it locally and wrote the runbook, then could not
deploy — Arc's non-interactive path needs a token, and its other path issues a
device code that waits for a human to click *Authorize this device*. Nobody is
watching an autonomous run, and the code expires in minutes.

## Provisioning the token

1. Mint a token in the PortalJS Arc dashboard, **scoped to the project it will
   deploy**. The cell user can read the credential file, and an agent with a
   shell is the cell user, so the blast radius is exactly what the token is
   allowed to do.

2. Add it to the encrypted secrets, which live in git as ciphertext.

   `sops` needs to be told where the private key is — the wrapper sets this
   for itself, so a bare `sops` fails with *"identity did not match any of the
   recipients"*:

   ```
   export SOPS_AGE_KEY_FILE=~/.config/datopian-workgraph/age-workgraph.key
   sops infra/secrets/staging.enc.yaml
   ```

   The key in the file is lower case, like every other key in it. The mapping
   to the environment variable Ansible reads lives in `with_secrets.sh`:

   ```yaml
   portaljs_token: arc_...
   ```

3. Deploy. The gastown role writes it to
   `/srv/cells/<cell>/.credentials/portaljs.env`, mode 0600, owned by the cell:

   ```
   cd infra/ansible
   ../../scripts/with_secrets.sh staging ansible-playbook -i inventory/staging.yml site.yml
   ```

With no token the file is not written, the deploy says so instead of skipping
in silence, and agents are told in their instructions that they cannot deploy —
so a run spends itself on the work it *can* do rather than rediscovering a 401.

## What the agent gets

`PORTALJS_TOKEN` (and `PORTALJS_API`, if set) arrive in the agent's
environment, where every Arc CLI and API call already looks for them. The
instructions say which case the run is in, and tell it not to start a device
flow either way.

## What agents deliberately do NOT get

Cloudflare credentials. `CLOUDFLARE_API_TOKEN` in the same secrets file is the
**infrastructure** token: terraform uses it for DNS, Cloudflare Access policies
and every R2 bucket. An agent holding it could reach production infrastructure
by mistake, so it stays out of cells. Arc is the sanctioned path and the
narrower grant.

If a direct-to-Cloudflare path is ever needed, mint a *separate* token limited
to Workers Scripts:Edit and one bucket on one account. Never reuse the
terraform one.

## Why two names for one secret

The file key is `portaljs_token` and the environment variable is
`PORTALJS_TOKEN`, mapped by hand in `scripts/with_secrets.sh`. That mapping is
written out rather than derived so a rename is a visible edit — and its cost is
that a forgotten line makes the lookup resolve to `""` on every deploy, so the
credential is silently never installed. `check_infra.py` now fails when a
`lookup('env', ...)` in `group_vars` has no matching export.

## Rotating

Replace the value in `infra/secrets/<env>.enc.yaml` and re-run the deploy. The
file is rewritten in place; nothing caches it, and the next run picks it up.
