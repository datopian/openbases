# Runbook: rotate or revoke a credential

Owning work package: WP-B3. Covers the nine credentials in `credential_registry`.

```sql
-- what exists, who owns it, what is overdue
SELECT name, owner_email, state, due_at FROM credential_rotation_due ORDER BY state, name;
```

## Before you start

Everything here needs the age key, because every credential lives in
`infra/secrets/<env>.enc.yaml`:

```bash
export SOPS_AGE_KEY_FILE=~/.config/datopian-workgraph/age-workgraph.key
```

Rotation is three things, and skipping any one of them leaves a half-rotation
that is worse than no rotation:

1. the provider issues or accepts a new value
2. the encrypted file holds it
3. the hosts are redeployed so the running processes use it

## Rotate

```bash
scripts/rotate_credential.sh staging <name>            # generates where it can
scripts/rotate_credential.sh staging <name> --value V  # where it cannot
```

The script re-encrypts, verifies the file still passes its own encryption check,
and then prints the provider steps **in the correct order for that credential**.
Read that ordering. It differs, and the difference is whether you cause an
outage:

| credential | order | why |
|---|---|---|
| `db_app_password` | PostgreSQL first, then deploy | the running pool stays open, so there is no window at all |
| `github_webhook_secret` | deploy, then GitHub, fast | **there is a window either way** — one secret is in force per side and they must match. GitHub retries, and the Advanced tab can redeliver. wg-4bn removes the window by accepting the previous secret too |
| `github_app_private_key` | add new, deploy, verify, **then delete old** | GitHub allows two valid keys. Stopping before the delete leaves more exposure than you started with |
| `ai_gateway_token`, `cloudflare_api_token` | create new, deploy, verify, then revoke old | Cloudflare cannot scope a gateway token to one gateway (wg-4r2) |

Then deploy, and record it only once the provider side is genuinely finished:

```bash
cd infra/ansible && ../../scripts/with_secrets.sh staging \
  ansible-playbook -i inventory/staging.yml site.yml

scripts/record_rotation.sh staging <name>
```

Recording is a separate command deliberately. A registry claiming a credential
is fresh while the old one still works is worse than one saying "never rotated",
because the first is believed.

### Confirming a rotation actually took

Check the **running process**, not the files. This is not pedantry — a play that
fails part-way applies its file changes and never runs its handlers, and the
next run then sees the files as correct and restarts nothing. That happened
here: a new binary, unit and environment file sat on disk for two days while the
old process served traffic with the old secret in its environment.

```bash
# which source did the running binary read from?
journalctl -u control-api --no-pager -o cat | grep "credential sources" | tail -1
# no secrets in the process environment at all
tr '\0' '\n' < /proc/$(systemctl show -p MainPID --value control-api)/environ | grep -cE 'PASSWORD|SECRET'
# and reconciliation, which shares the DSN
systemctl start workgraph-reconcile.service && journalctl -u workgraph-reconcile -n 5 --no-pager -o cat
```

## Revoke — the stolen-laptop path

```bash
scripts/revoke_credential.sh staging <name>
scripts/revoke_credential.sh staging --all      # takes the control API down
```

This shreds the credential source file and restarts the unit. **The service will
fail to start, and that is the correct outcome** — a service still running on a
credential you are revoking has not been revoked. Verified on staging:

```
control-api.service: Failed to set up credentials: Protocol error
Main process exited, code=exited, status=243/CREDENTIALS
```

`243/CREDENTIALS` is what to grep for; "Protocol error" alone does not say which
credential is missing.

Restarting the unit matters as much as deleting the file. The credential lives in
a tmpfs the unit mounts, so a process that has already started still holds its
copy — removing the file alone revokes nothing from it.

**Removing it here only stops this system using it.** Anyone else with a copy is
unaffected until you revoke at the provider; the script prints where for each
credential. That is the step that actually ends the credential's life.

Restore by rotating and redeploying.

## If the age key is lost

There is no recovery path in this repository. `infra/secrets/*.enc.yaml` becomes
unreadable, and every credential must be reissued at its provider and a new age
key generated.

Plan §11.4 requires an offline copy held with Datopian leadership alongside the
OpenTofu state passphrase. That copy is the recovery path, and it is a human
responsibility this tooling cannot discharge — tracked on wg-8yv.5.
