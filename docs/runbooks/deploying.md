# Runbook: deploying to staging

**Deploys are manual.** There is no CI deploy: it would need the age private
key in GitHub Actions to decrypt the secrets, and putting it there is a worse
trade than typing a command. There used to be a `staging-deploy` workflow stub
gated on an unset variable; every merge SKIPPED it, and a skipped job shows a
green tick, so main looked deployed when nothing had shipped (wg-ge1). It was
deleted rather than left to lie. If a real CI deploy is ever built, it runs the
command below on a runner that can reach the nodes and holds the age key.

## The command

```bash
make build-linux                      # ALWAYS first; see below
cd infra/ansible
../../scripts/with_secrets.sh staging \
  ansible-playbook -i inventory/staging.yml site.yml
```

Run it from `infra/ansible`: `ansible.cfg` carries the ProxyCommand that reaches
the nodes over the tunnel. Run it through `with_secrets.sh`: it decrypts
`$WG_SECRETS_DIR/staging.enc.yaml` into one child process.

It takes about eleven minutes. The tunnel drops on long silences, so run it in
the background and read the recap rather than watching it.

## Why `make build-linux` first

Every `*_local_binary` resolves through `first_found` against `bin/linux-amd64`
and falls back to `""`, and every install task skips on empty. Forget the build
and the deploy installs **nothing**, reports `failed=0`, and leaves the nodes on
the previous binaries. That happened on 9 September: an 11m02s run with
`failed=0` shipped no new `wg-runner`, and the next tagged run changed 14 files
on the control node because those were stale too.

A play at the front of `site.yml` now refuses a deploy whose build directory is
empty. The first line of a healthy run is:

```
"msg": "15 binaries ready to install"
```

If you see the refusal instead, run `make build-linux` and start again.

## Deploying less than everything

```bash
# Go binaries only — the common case after a code change.
../../scripts/with_secrets.sh staging \
  ansible-playbook -i inventory/staging.yml site.yml --tags binaries

# One host.
... --limit workgraph-staging-execution
```

```bash
# Migrations only — schema change with no code change.
../../scripts/with_secrets.sh staging \
  ansible-playbook -i inventory/staging.yml site.yml --tags migrate
```
Use `--tags migrate`, NOT `--start-at-task "Install the migration tool"`. The
control-api binary is installed BEFORE the migration tool, so starting at the
migration task applies the migrations and leaves the API running the previous
code -- silently: migrations land, `failed=0`, and only `/version` says the
code is stale (wg-ge1). `--tags migrate` runs the three migration tasks and
nothing else, on purpose.

`--tags binaries` covers the binaries and the units that carry them. It does
**not** cover apt packages, credential files or the dispatcher unit template —
those live in role tasks with no tag, so a change to any of them needs the full
run. Two examples that caught this out: installing `git-lfs`, and writing the
PortalJS Arc credential.

## Changing the web UI

The web application is embedded in the Go binary, so building the binary is not
enough:

```bash
make web-build      # builds apps/web and copies it into internal/webui/dist
make build-linux
```

Skip `web-build` and the API serves the previous UI while reporting a successful
deploy. Confirm which bundle is live:

```bash
grep -o 'index-[A-Za-z0-9_-]*\.js' internal/webui/dist/index.html
./scripts/on.sh staging control "curl -s http://127.0.0.1:8080/ | grep -o 'index-[A-Za-z0-9_-]*\.js'"
```

The two must match.

## Afterwards

The recap is the evidence. `failed=0` on every host, and `changed=` reflecting
what you actually altered — a deploy that changes nothing when you expected it
to change something is the failure this runbook exists to make visible.

To confirm a binary really landed, ask the node rather than the playbook:

```bash
./scripts/on.sh staging execution "stat -c '%y %s' /usr/local/bin/wg-runner"
ls -l bin/linux-amd64/runner
```

Sizes should match.
