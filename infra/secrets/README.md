# Secrets

Encrypted with [SOPS](https://github.com/getsops/sops) and [age](https://age-encryption.org).
The `.enc.yaml` files in this directory are safe to commit: every value whose key name matches
`encrypted_regex` in `/.sops.yaml` is ciphertext.

## Reading and editing

```bash
export SOPS_AGE_KEY_FILE=~/.config/datopian-workgraph/age-workgraph.key
sops infra/secrets/staging.enc.yaml          # decrypts, opens $EDITOR, re-encrypts on save
sops -d infra/secrets/staging.enc.yaml       # print the decrypted document
```

Never `sops -d > somefile`. A decrypted copy on disk is the thing this directory exists to
prevent, and it will be committed by someone eventually.

## What is and is not encrypted

Only values whose **key name** matches the pattern — `*_token`, `*_secret`, `*_password`,
`*_passphrase`, `*_key`, `*_credential`. Everything else stays readable so that a diff is
reviewable.

That makes the naming a security boundary rather than a convention. `github_app_id` is an
identifier and is stored in clear; `github_webhook_secret` is a credential and is not. If you add
a value, the name decides, so name it accurately. `scripts/secret_scan.sh` fails the build if a
credential-shaped value ends up in clear.

## The private key

`~/.config/datopian-workgraph/age-workgraph.key`, mode 0600, outside every repository.

**Losing it means losing the ability to decrypt these files.** Plan §11.4 requires an offline copy
held with Datopian leadership, alongside the OpenTofu state passphrase. That is a human action and
is tracked on wg-8yv.5 — the mechanism here does not substitute for it.

The public recipient is in `/.sops.yaml`. To add a second holder, add their public key as another
`age:` recipient and run `sops updatekeys infra/secrets/*.enc.yaml`.

## Rotation and revocation

See `docs/runbooks/rotate-a-credential.md`.
