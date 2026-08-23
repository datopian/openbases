# Self-hosted CI runners for Datopian repositories

One Hetzner box runs GitHub Actions for every opted-in Datopian repository.

## Why

GitHub bills hosted runners per minute, so cost rises with use and a busy month
hits a spending limit and blocks **every** repository. That is not hypothetical:
**$35 of minutes over 143 job-hours in August**, then CI stopped mid-work, and
raising the limit only moves the cliff.

A machine we own costs the same whether it runs one job or ten thousand:

| | GitHub hosted | One cx33 |
| --- | --- | --- |
| Cost | ~$0.245 per job-hour | **€8.49/month, flat** |
| August's 143 job-hours | $35 (truncated by the limit) | €8.49 |
| Ten times that usage | ~$350 | €8.49 |
| Capacity | unlimited, metered | ~1,440 job-hours/month |
| Spec | 2 vCPU / 7 GB | 4 vCPU / 8 GB |

The saving is secondary. The point is that cost stops being a function of usage,
so CI cannot be switched off by a budget again.

## Why not a VM per job

That is the pattern in `datopian/postal-codes`, and it is right *there*: a
monthly batch job, one hour, a cent. It is wrong for CI on every pull request.
Hetzner bills hourly, so a five-minute job and a one-hour job cost the same —
fifty jobs a day would cost **more** than a permanently running box, and each
would pay 60–90 seconds of boot before starting.

That workflow also pins `stonemaster/hetzner-github-runner@HEAD`: a floating
reference to an **archived**, unlicensed, 14-star repository, handed both a
GitHub API key and a Hetzner API key. Worth pinning to a commit SHA there
regardless of what we do here.

## Shape

- **Its own Hetzner project.** A token can delete every server in its project.
  CI is the least trusted thing we run, and the Workgraph control and execution
  nodes must not be one compromised workflow away from deletion.
- **No inbound ports.** The runner long-polls GitHub outbound. Management is a
  Cloudflare tunnel, same as every other node here.
- **Ephemeral registrations.** Each runner takes one job, deregisters and exits;
  systemd restarts it and it registers afresh. No job inherits the previous
  job's runner.
- **An organisation runner group**, scoped to named repositories. Never "all
  repositories", never a public one — a fork's pull request would be enough.

## What this is NOT

**Not container isolation.** Jobs run as one user on a shared kernel. A job that
deliberately attacks the host can affect later ones. Today the group holds
private repositories whose contributors already have commit access, so the
threat is accident rather than malice — but "ephemeral" is easy to read as
"sandboxed", and it is not. If untrusted code ever runs here, the next step is a
container or a VM per job.

**Not highly available.** One box is a single point of failure for every
repository's CI. Accepted at this size; a second is another €8.49 and the module
takes a count.

**Not free to run.** Runner upgrades, Docker layers filling the disk, and the box
being down meaning all CI is down. Perhaps an hour a month once stable, more
while it is not.

## Migrating a repository

`runs-on: ubuntu-latest` will **not** use these runners. GitHub always routes
that label to its own fleet and it cannot be shadowed. Every job has to change:

```yaml
jobs:
  build:
    runs-on: [self-hosted, linux, x64]   # was: ubuntu-latest
```

That is the real migration cost — one line per job across every repository, and
new repositories default silently back to paid hosted runners unless someone
notices.

Things that differ once migrated:

- **`services:` needs Docker**, which is installed here. Workgraph's `database`
  job uses postgres with pgvector and simply cannot run on a runner without it.
- **No clean image per job.** Ephemeral registration plus a wiped work directory
  gets most of the way; installed system packages persist.
- **`setup-*` actions still work** but download each time unless a tool cache is
  warmed.

## Operating it

```bash
# reach it
ssh -o ProxyCommand="cloudflared access ssh --hostname %h" root@ssh-ci-runner.openbases.com

# what the runners are doing
systemctl status 'ci-runner@*'
journalctl -u 'ci-runner@1' -f

# add capacity: raise ci_runner_count, or add a second host
```

Registrations are minted per job from a GitHub App (preferred — its tokens last
an hour) or a PAT. The long-lived credential is a systemd credential, root-only,
and never reaches the runner user.

## Deploying

```bash
scripts/with_secrets.sh ci scripts/tofu.sh ci apply
cd infra/ansible && ../../scripts/with_secrets.sh ci \
  ansible-playbook -i inventory/ci.yml ci.yml
```
