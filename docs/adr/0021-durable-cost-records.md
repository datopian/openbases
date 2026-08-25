# ADR-0021: Spend is recorded in the database, not read from the gateway

Status: accepted
Date: 2026-08-25
Bead: wg-2a0

## Context

Before this, every cost question was answered by `scripts/cost_by_role.py`, which
calls the Cloudflare AI Gateway logs API and prints a table. That has three
problems, in increasing order of seriousness.

It is slow and rate-limited. The API pages at 50 entries, so a month of traffic
is sixty-odd round trips for a number that does not change.

It cannot be checked. The script prints a total; nothing keeps the rows it
summed. Two people running it a week apart get different answers and neither can
say why.

**The evidence expires.** The gateway log rotates on DELETE_OLDEST at ten million
entries. Spend older than the window is not archived, not exported and not
recoverable — it is gone, on Cloudflare's schedule rather than ours. A budget
conversation about last quarter is a conversation about data that no longer
exists.

And the log cannot answer the question the plan actually asks. A log entry has a
model and a cost. It has no project and no bead, so "what did this project cost"
and "what did this piece of work cost" are not slow to answer — they are
unanswerable.

## Decision

Import the gateway log into `usage_records` on a timer, and answer cost questions
from the database.

Four decisions inside that are worth stating, because each has an obvious wrong
answer.

**Cost is `numeric(16,8)` cents, not integer cents.** `usage_records.cost_cents`
was `integer`. A typical Haiku request costs $0.000088, which is 0.0088 cents,
which rounds to zero. 2,236 such requests round to zero as well: the table would
have confidently reported that a month of real traffic cost nothing. That is
worse than having no table, because it looks like an answer. The conversion runs
through `big.Float` at 80 bits and is carried to PostgreSQL as a decimal string,
so no float rounding happens in the driver either.

**The gateway's own id is the idempotency key.** Each row carries `external_id`
with a unique index, and the write is `ON CONFLICT DO NOTHING`. The obvious
alternative — resume strictly after the last imported timestamp — silently drops
any entry that arrives out of order, and gives no way to recover from a partial
run. With an idempotency key the importer can deliberately re-read a window
behind its watermark, which it does: two hours by default.

**Cached and failed requests are recorded, and flagged.** A cache hit costs
nothing and is the only evidence the cache is working; dropping it makes the
saving invisible. A failed request often cost money before it failed, and
dropping it hides spend that produced no result — exactly the spend worth
finding. Both are columns, so a report can exclude them; neither is discarded
here.

**Untagged spend stays unattributed.** Where a request carries `cf-aig-metadata`,
the cell resolves to a project. Where it does not, `project_id`, `role`, `cell`
and `rig` are all NULL. Assigning untagged spend to a default project would make
the numbers add up and be wrong, and a wrong attribution is much harder to notice
than a missing one.

The importer writes through `system_record_usage`, a SECURITY DEFINER function.
It runs on a timer with no user, and `usage_records` has an RLS INSERT policy of
`WITH CHECK (current_app_user() IS NOT NULL)` — so a direct insert is refused
outright. This is the fourth component to need a system path (after
reconciliation, the monitor and the alert writer), which is enough repetition to
make it the default: anything that runs without a user goes through a narrow
SECURITY DEFINER function that cannot read a row back.

## Consequences

Spend became durable and joinable. The first real import moved 3,293 entries and
$50.71 out of a rotating log and into a table, and re-running it imported nothing
new.

Two gaps are now visible that were invisible before, which is the point:

**All 3,293 imported rows are unattributed.** The tagging mechanism exists — Gas
Town writes `cf-aig-metadata: {"role":...,"cell":...}` into every agent's
settings — but every imported entry predates its deployment. Nothing has run
through the gateways since 2026-08-17.

**`execution_cells` is empty on staging**, so even a tagged request has no cell
row to resolve, and therefore no project. Attribution cannot work end to end
until the cells that exist on disk are registered in the database. Tracked
separately; the import is correct either way and will attribute retroactively for
any entry it has not yet seen.

Unattributed rows are readable by any authenticated user. `usage_records_read`
admits a row whose `project_id IS NULL` to anyone with a session — the same rule
every project-scoped table uses. While attribution coverage is low that is nearly
all the spend. `test/acceptance/cost_import.sh` asserts the current behaviour, so
a change to it is visible rather than silent.

## Alternatives considered

**Keep querying the API and cache the result.** Cheaper to build, and it fixes
the speed problem. It fixes neither of the two that matter: the evidence still
expires, and a cache still cannot join to a project.

**Export the log to R2 via Cloudflare's log store.** Durable, and no code. But an
object in a bucket is not queryable next to `projects` and `work_refs`, which is
the whole requirement — and the log store is shared across gateways, so it does
not even separate environments.

**Write usage at the point of the request, from the agent.** Most accurate
attribution, since the caller knows its own bead. It also means every agent needs
database credentials and a working write path before it can make a model call,
and an agent that fails to write its usage either blocks or loses the record.
Importing after the fact is weaker on attribution and much stronger on not losing
data. Nothing prevents doing both later: `external_id` makes the import
idempotent against rows another writer inserted.
