# ADR-0022: Budgets are checked before dispatch, and refuse when they cannot be checked

Status: accepted
Date: 2026-08-26
Bead: wg-qw1

## Context

ADR-0018 gave each agent role a model tier, and ADR-0021 made spend durable and
joinable. Neither of them stops anything. The only ceiling that actually existed
was the per-gateway spend pool on Cloudflare, which returns 429 once the pool is
dry.

That pool is real protection and it is the wrong shape. It is per gateway, so
`workgraph-staging-oss` is one number covering every project that shares it; it
knows nothing about a project, and nothing at all about a piece of work.
`test/drills/budget_exhaustion.sh` exists because running into it is not even
cheap: rejected requests are free, but the stack around them retries, restarts
sessions and re-primes context, which costs money on every attempt while no
attempt can succeed.

`budget_limits` had existed since 0006 with zero rows, nothing reading it, an
`integer` cents column, and a constraint permitting exactly one of `project_id`
or `execution_cell_id` — so "this piece of work may cost two dollars" could not
be written down at all.

## Decision

Check the budget before dispatch, at the point where money starts being spent,
and refuse when the answer cannot be trusted.

**The most specific budget wins, and spend is summed at the same level.** A
bead's budget, else its project's, else its cell's. That pairing is the whole
design and it lives in one function (`system_budget_status`) rather than two,
because if resolution and summation ever disagree the failure is silent: ten
beads each compared against a project's whole ceiling would let each of them
spend all of it.

**Spend is attributed to a bead by tagging the request, not by inferring from
time.** `cf-aig-metadata` already carried role, cell and rig, written at deploy
time because those are properties of a cell. A bead is a property of one
dispatch, so `wg-tag-bead` writes it immediately before the sling and clears it
in the teardown. The alternative — sum the cell's spend between a run's start and
end — needs no new mechanism and quietly misattributes the moment two beads run
in one cell.

**A check that cannot be made is a refusal, not an allowance.** Spend arrives
through an hourly import, so the answer is always somewhat behind. Three
distinct not-knowing cases are all refusals:

- data older than `WG_BUDGET_MAX_STALENESS` (2h by default: the importer runs
  hourly with a two-hour overlap, so one missed run is still trusted and two are
  not);
- nothing ever imported, which is *not* the same as nothing spent;
- the control plane unreachable from the dispatcher.

Failing open on any of these would mean the budget silently stops existing
exactly when the platform is unwell.

**But an exceeded budget refuses regardless of staleness.** More spend has
happened since the import, never less.

**And unbudgeted work is allowed, loudly.** The alternative is that turning
enforcement on stops all work until somebody has enumerated every project, which
is how enforcement gets switched off wholesale on its first day. Every allow of
unbudgeted work carries a warning saying so.

**The dispatcher asks the control plane; it does not read the database.** The
execution node holds no database credential and should not start. It already
holds an Access service token, so this is a third narrow Access application on
that token, alongside the git-credential and agent-health ones — an application
matches one domain, and widening one to a prefix would extend the token to
everything under it. This endpoint only reads, which makes it the smallest of the
three powers, and its own audience is what keeps it that way.

## Consequences

Enforcement exists where it did not. `scripts/dispatch_bead.sh` refuses before
slinging, names the budget and the amount, and prints the two commands that
resolve it — raise the ceiling, or override with `--no-budget-check`. The
override is deliberately awkward to type, because an override that has to be
argued for in the moment is better than one people replace by deleting the check.

The staleness rule bites immediately on staging, and correctly. The gateway logs
stop at 2026-08-17 and `wg-costimport` is stopped for want of a scoped token
(wg-oku), so the first real `wg-budget status` said:

    decision    REFUSE — spend data is 216h49m0s old, older than the 2h0m0s
                this check trusts; the budget cannot be checked

Staging therefore runs with `WG_BUDGET_MAX_STALENESS=0` until the importer is
running: the ceilings still apply, only the freshness requirement is lifted. That
is a deliberate, recorded, temporary setting rather than a permanently disabled
check.

`wg-budget list` reading `budget_limits` directly was the fifth instance of the
RLS-with-no-user trap in this codebase — `set` reported success and `list`
reported "no budgets are set", both truthfully from where they stood. The rule is
now unambiguous and written into 0031: anything running without a user reads
through a SECURITY DEFINER function, *including the parts that only display
things*.

Two limits are recorded and still unenforced: `max_concurrent_agents` and
`max_runtime_minutes`. They are columns on a budget, they are set by
`wg-budget set`, and nothing reads them — `agent_max_runtime_minutes` is enforced
by a systemd timer with a node-wide value, and concurrency is not enforced at
all. Recorded here rather than left to be discovered.

## Alternatives considered

**Enforce in the AI Gateway.** It is the only place that can refuse mid-run, and
it cannot express per-bead: a gateway is per trust tier and its pool is a single
number. Keeping it as the outer bound and adding a finer one above it is why both
exist.

**Post-hoc detection: import, sum, alert, stop future dispatch.** Much simpler,
no new endpoint, no tagging. It is also always late by construction — the run
that blew the budget has already finished — and the thing being protected against
is a runaway that spends the month's budget in an afternoon.

**Have the agent report its own spend as it goes.** The most accurate and the
most fragile: every agent would need a write path before it could make a model
call, and an agent that fails to report either blocks or loses the record.
Importing after the fact is weaker on latency and much stronger on not losing
data.
