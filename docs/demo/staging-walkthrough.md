# Demoing staging end to end

What this shows: a project brief goes in, an agent turns it into beads, a bead
gets dispatched to a real agent on a real machine, and the cost of that agent
lands against the cell that spent it. That is the whole system in one loop —
work intake, planning, execution isolation, and cost attribution.

Everything is at **https://work-staging.openbases.com**. You will hit
Cloudflare Access first and log in as yourself; that identity is what the API
uses to decide what you can see.

## Before you start

Nothing. It is all up.

The `/v1/node/` Access application exists, `wg-dispatcher` is running on the
execution node, and the loop below has been run end to end — a brief in, two
linked beads out, 18.57 cents attributed to the run that produced them.

`scripts/on.sh` in the commands below runs one command on a node. Neither node
has an inbound port, so plain `ssh` will not work unless your ssh config already
carries the cloudflared ProxyCommand; the script borrows Ansible's, which does.

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

**Measured, not estimated:** the dispatcher polls every ten seconds, and the
planning run itself took 26 seconds. Allow about a minute and a half from
pressing the button to seeing the beads. The page polls every five seconds.

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

**Watch the cents column** — but first make it appear. Spend is imported from
the gateway on an hourly timer, so straight after a run the column still reads
`0` and looks broken. Force the import:

```bash
scripts/on.sh staging control 'systemctl start wg-costimport.service'
```

Then refresh. The last run through this loop landed as:

```
bead                                       role     model             calls  cents
plan-d37e5ff7-79aa-4040-9436-368f7a347b7a  polecat  claude-sonnet-5       6  18.57
```

That number came from the model gateway and was matched back to this bead by the
headers the runner set — which is why cost lands on a *bead* and a *role*, not
just on an account. Say the hourly lag out loud rather than letting someone
notice a zero and draw their own conclusion.

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

## If the dispatcher is down

The read half does not depend on the execution node at all. From the control
node:

```bash
scripts/on.sh staging control 'wg-env wg-work sync-hq'
```

That projects the HQ graph into `work_refs` directly, and the Work page then
shows the real backlog — status, ownership, spend per bead. Do not press **Plan**
in this state: the job queues and nothing claims it.

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

**Silence is the healthy state.** It logs when it starts, when it runs a job and
when one finishes, and nothing in between — an idle queue produces no output at
all, so an empty journal is not a symptom.

If the service token is not accepted for a path, you get this rather than a 401,
because Cloudflare Access answers an unauthenticated request with **200 and its
login page**:

```
expected JSON, got "text/html; charset=utf-8" — this is Cloudflare Access
serving its login page, which means the service token was not accepted for
this path
```

That means the AUD in `group_vars/all/secrets.yml` no longer matches the Access
application. Compare it against
`scripts/with_secrets.sh staging scripts/tofu.sh staging output -raw cell_work_queue_aud`.

A job that fails reports why, in the queue and in the UI — for example
`a run needs the AI Gateway token, or its spend escapes every budget`, which is
`wg-runner` refusing to run outside the budget rather than failing open. A job
stuck *in flight* times out on its own after fifteen minutes and reports the
failure rather than sitting there forever.
