# My CI job failed with "Actions budget" — what to do

You hit the GitHub Actions spending limit. **This is deliberate.** The limit is
set low on purpose so that a job which could run on our own runner does not
quietly get billed per minute instead.

## The fix, one line per job

In `.github/workflows/*.yml`, change:

```yaml
jobs:
  build:
    runs-on: ubuntu-latest        # ← billed per minute by GitHub
```

to:

```yaml
jobs:
  build:
    runs-on: [self-hosted, linux, x64]
```

Push. That's it — the job runs on Datopian's own runner and costs nothing per
minute.

`ubuntu-latest` cannot be redirected to our runners: GitHub always routes that
label to its own fleet, and the label cannot be shadowed. So every job that
should run on ours has to say so.

## What is different on our runner

Mostly nothing. It is Ubuntu with Docker, and `actions/checkout`, `setup-go`,
`setup-node` and `setup-python` all work.

Four differences worth knowing:

**Service containers work.** Docker is installed, so `services:` blocks —
postgres, redis — run as they do on GitHub.

**The machine is not fresh.** GitHub gives every job a brand-new VM; ours is
shared and long-lived. The work directory is cleaned per job, but system
packages someone's workflow installed are still there. Do not rely on a clean
image, and do not `apt install` things that other repositories would be
surprised by.

**Concurrency is finite.** There are a fixed number of runner slots. If they are
busy your job queues rather than failing. A job that hangs holds a slot, so add
`timeout-minutes` to anything that could.

**Nothing secret should be left behind.** Write secrets to the environment, not
to files in the work directory, and never to `/tmp`. Other repositories' jobs
run on the same box.

## What must stay on GitHub's runners

- **macOS or Windows.** We only have Linux.
- **Public repositories.** They are free on GitHub's runners, and a fork's pull
  request must never run on our hardware.

For those, ask for the budget to be raised rather than working around it.

## If it still does not run

Check the labels match. `runs-on: [self-hosted, linux, x64]` needs a runner
advertising all three. If the job sits queued for minutes, the runners may be
full or down:

```bash
ssh -o ProxyCommand="cloudflared access ssh --hostname %h" root@ssh-ci-runner.openbases.com
systemctl status 'ci-runner@*'
```

Ask in the engineering channel before disabling the limit — a blocked job is
usually a one-line change, and raising the limit is how the bill grew in the
first place.
