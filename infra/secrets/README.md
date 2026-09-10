# Secrets

Encrypted with [SOPS](https://github.com/getsops/sops) and [age](https://age-encryption.org).

**This directory carries templates, not secrets.** `*.example.yaml` names what a deployment
needs and holds nothing. The real `*.enc.yaml` files live outside this repository, by default in
`~/.config/datopian-workgraph/secrets/`; `WG_SECRETS_DIR` overrides that.

## Why not commit the encrypted files

SOPS ciphertext IS safe to commit, and committing it is a mainstream practice — one reviewed
copy, versioned with the code, recovery is a clone plus one key. This repository did exactly
that until it went public.

Public changes the calculus, and not because the cryptography weakens:

- **Permanence.** A public repository is archived by people you do not know. If the age key ever
  leaks, every archived version decrypts retroactively, including values rotated years earlier.
  Rotation normally caps a key leak's window; published ciphertext has no window to cap.
- **Metadata.** Perfect encryption still publishes which secrets exist, their names, when each
  last changed, who can decrypt, and whatever identifiers sit in clear beside them — a map of the
  Cloudflare account, the zone, the GitHub App and the Hetzner project, without decrypting
  anything.
- **Review cost.** "Secrets directory in a public repository" is a heuristic an auditor has to
  disprove, every time, forever.

None of those is an argument against SOPS. They are arguments against publishing the file, so the
file is not published.

## Reading and editing

```bash
export SOPS_AGE_KEY_FILE=~/.config/datopian-workgraph/age-workgraph.key
export WG_SECRETS_DIR=~/.config/datopian-workgraph/secrets     # the default

sops "$WG_SECRETS_DIR/staging.enc.yaml"      # decrypts, opens $EDITOR, re-encrypts on save
sops -d "$WG_SECRETS_DIR/staging.enc.yaml"   # print the decrypted document
```

That directory needs its own `.sops.yaml`, because SOPS finds creation rules by walking up from
the file it is reading and there is no repository above it. Copy the block from `/.sops.yaml`
and widen the path so it matches where the file now is:

```yaml
creation_rules:
  - path_regex: .*\.enc\.yaml$
    encrypted_regex: "^(.*_token|.*_secret|.*_password|.*_passphrase|.*_key|.*_credential)$"
    age: age12lv6dvkvevth3gkl4g0jpw7kxqs4slkl6vk8vp8675hpq2mmw9cq3cc9c4
```

## Creating one from the template

```bash
printf 'environment: staging\n' > /tmp/seed.yaml
sops -e --filename-override staging.enc.yaml /tmp/seed.yaml > "$WG_SECRETS_DIR/staging.enc.yaml"
shred -u /tmp/seed.yaml
sops "$WG_SECRETS_DIR/staging.enc.yaml"      # add the rest in $EDITOR
```

`--filename-override` is not optional. SOPS chooses its rules from the path of the file it is
READING, so `sops -e /tmp/staging.yaml` matches no rule, encrypts **nothing**, and writes a file
that looks encrypted because it still carries a `sops:` block.

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
