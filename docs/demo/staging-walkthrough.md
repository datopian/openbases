# Demoing staging end to end

What this shows: a project brief goes in, an agent turns it into beads, a bead
gets dispatched to a real agent on a real machine, and the cost of that agent
lands against the cell that spent it. That is the whole system in one loop —
work intake, planning, execution isolation, and cost attribution.

`scripts/on.sh` below runs one command on a node. Neither node has an inbound
port, so plain `ssh` will not work unless your ssh config already has the
cloudflared ProxyCommand; the script borrows Ansible's, which does.

Everything else is at **https://work-staging.openbases.com**. You will hit
Cloudflare Access first and log in as yourself; that identity is what the API
uses to decide what you can see.

## Before you start: one apply

There is one piece I could not put in place, because it creates a Cloudflare
Access application and I am not making Cloudflare changes outside of a plan you
have seen. It takes about a minute.

```bash
cd ~/code/workgraph
scripts/with_secrets.sh staging scripts/tofu.sh staging apply
scripts/with_secrets.sh staging scripts/tofu.sh staging output -raw cell_work_queue_aud
```

Take the AUD that prints and put it in `infra/ansible/group_vars/all/secrets.yml`,
where the key is already there waiting, empty:

```yaml
control_api_cell_work_access_aud: "<the aud>"
```

That file is plain YAML, not encrypted — an AUD says which application a token
was issued for and grants nothing on its own.

Then redeploy both nodes:

```bash
cd infra/ansible
../../scripts/with_secrets.sh staging ansible-playbook -i inventory/staging.yml site.yml
```

The dispatcher on the execution node enables itself on that run. It stays off
until the AUD is set, deliberately: started earlier it would just log a 401
every fifteen seconds and bury the next real failure.

**What this unlocks:** the execution node's ability to claim queued work. The
node has no inbound port — that is deliberate — so it reaches out to
`/v1/node/...` to ask for a job, and that path needs its own Access application
with a service-token policy. Without it the queue accepts jobs and nothing
picks them up.

**If the apply fails or you are short on time**, skip to
[The read-only demo](#the-read-only-demo). It needs none of this and still shows
real data.

## The full loop

### 1. Open the Work page

Go to https://work-staging.openbases.com and click **Work** in the nav.

You should see a table of beads with status, the agent that ran each one, and
what it cost in cents. This is the `oss` cell's real graph — around 100 beads
projected from the HQ repository, not fixtures.

### 2. Give it a brief

In the box at the top, type something a person would actually say. For example:

> Add a health check endpoint to the control API that reports database
> connectivity and the last successful cost import, and wire it into the
> external probe.

Press **Plan**. A row appears under *In flight*.

**What is happening:** the brief became a row in `work_queue`. Within ten
seconds `wg-dispatcher` on the execution node claims it, and `wg-runner` starts
a Claude Code agent inside the `oss` cell's cgroup slice with its own Dolt
database. The prompt tells it to file work, not do it, and bounds it to between
two and eight beads that each need acceptance criteria.

Expect **60 to 120 seconds**. The page polls every five seconds.

### 3. Read what it filed

When the row moves to *Recently finished*, its result text is the agent's own
account of what it created. Then the new beads appear in the table below,
because the dispatcher pushes the cell's bead state up on every pass.

**The point to make out loud:** nobody wrote those bead IDs. The agent decided
the decomposition, and it is now sitting in the same graph as the work a human
filed.

### 4. Dispatch one

Pick one of the new beads and press **Dispatch**. It queues, the dispatcher
claims it, and an agent does the work in the cell.

**Watch the cents column.** It fills in after the run. That number came from the
model gateway, matched back to this bead by the headers the runner set — which is
why cost lands on a *bead*, not just on an account.

### 5. Show what happens at the limit

This is the part worth demoing, because it is what makes any of this safe to run
unattended. Pick a bead you are not about to dispatch for real — `wg-7kf` below
— and put a budget of zero on it:

```bash
scripts/on.sh staging control 'wg-env wg-budget set bead wg-7kf 0'
scripts/on.sh staging control 'wg-env wg-budget status wg-7kf --cell oss'
```

The status prints, and the command exits non-zero:

```
budget      bead wg-7kf, 0 cents per day
spent       0 cents today, 0 remaining
decision    REFUSE — the bead budget for wg-7kf is spent: 0 of 0 cents used today
```

Now press **Dispatch** on that bead in the UI. It refuses, and shows that same
reason. Then give it back:

```bash
scripts/on.sh staging control 'wg-env wg-budget unset bead wg-7kf'
```

**Two things worth saying out loud.**

The gate is checked before an agent starts, not after it has spent the money.
An over-budget bead fails to dispatch rather than getting killed mid-run with a
half-finished commit.

And budgets resolve most-specific-first: a bead's own, else its project's, else
its cell's. That is why the demo sets one on the *bead* — `oss` beads are under
the `portaljs-oss` project budget, so lowering the cell ceiling would not touch
them. `unset` removes a level and restores the fallback; zero does not, because
zero is a real budget meaning refuse everything.

## The read-only demo

If the apply above did not happen, this still works and is worth showing.

On the control node:

```bash
scripts/on.sh staging control 'wg-env wg-work sync-hq'
```

That projects the HQ graph into `work_refs` directly from the control node. Then
the Work page shows the real backlog — status, ownership, spend per bead. The
brief box will queue jobs that nothing claims, so do not press **Plan** during a
read-only demo.

## Other things worth showing

- **Cost** — the Costs page has real gateway spend, about $50 imported across
  3,293 entries, attributed by role, cell and bead.
- **Alerts** — the Alerts page covers seven classes including service health and
  a stale cost import. `test/acceptance/alerting.sh` proves 12 of 12.
- **The isolation claim** — while an agent is running:

  ```bash
  scripts/on.sh staging execution 'systemd-cgls /wgcell-oss.slice'
  ```

  It shows the agent inside the cell's CPU and memory limits. Two cells cannot
  starve each other.

## If something is wrong

The dispatcher is the moving part. On the execution node:

```bash
scripts/on.sh staging execution 'journalctl -u wg-dispatcher-oss -n 50 --no-pager'
```

A repeating 401 means the AUD from the apply is missing or does not match. A
repeating "no work" is normal and means the queue is empty.

If a job is stuck *in flight*, it will time out on its own after fifteen minutes
and report the failure rather than sitting there forever.
