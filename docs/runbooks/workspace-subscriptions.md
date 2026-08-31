# Google Workspace event subscriptions

What keeps Drive and Meet discovery working, how it fails, and what to do.

## What runs

`workgraph-workspace.timer` on the control node, hourly, running
`/usr/local/bin/workgraph-workspace` (built from `cmd/workspaced`). Each pass:

1. Reads the allow-list and what we believe, through `system_event_sources()`.
2. Asks Google what it actually holds, and adopts or deletes what does not match.
3. Decides one action per source: none, create, renew, update, reactivate, replace, delete.
4. Applies each and writes back the result — including failures.

It is idempotent. A second pass immediately after a first applies nothing, and
that property is asserted in `scripts/accept_workspace_events.sh` step 3.

## Why hourly, and why that is not a preference

Google expires a Workspace Events subscription within days. `DefaultPolicy`
renews inside 24 hours of expiry, and `Policy.Validate` refuses any policy whose
renewal window does not exceed the reconciliation interval — because a
subscription that expires between two runs which each saw it as healthy stops
delivering with nothing to notice.

So the timer interval is load-bearing. Raising `OnUnitActiveSec` above 24h makes
the reconciler refuse to run at all, which is deliberate: loudly broken beats
quietly deaf.

## Checking it

```bash
scripts/on.sh staging control 'systemctl list-timers workgraph-workspace.timer --no-pager'
scripts/on.sh staging control 'journalctl -u workgraph-workspace.service -n 50 --no-pager'
scripts/on.sh staging control '/usr/local/bin/workgraph-workspace -summary -since 168h'
```

`-summary` reports deliveries per source and event type. Zero for a Meet source
is normal: the meeting is sometimes skipped and sometimes held with
transcription off. Zero across every source for a week is not.

A dry run decides and prints without calling Google or writing anything, and
needs no credential:

```bash
scripts/on.sh staging control '/usr/local/bin/workgraph-workspace -dry-run -v'
```

## Failures, and what each one means

**`google refused the assertion: unauthorized_client`** — the delegation is
missing or a scope differs by a character. Delegation is granted in the
Workspace admin console against the service account's `oauth2ClientId`
(`102065960434834149554`), not against its email, and it survives key rotation.
The two scopes are exactly:

```
https://www.googleapis.com/auth/drive.readonly
https://www.googleapis.com/auth/meetings.space.readonly
```

**`google refused the assertion: invalid_grant`** — `WG_GOOGLE_SUBJECT` is not a
real user in the domain. A service account cannot hold Drive or Meet resources
of its own, so every call is made as a person.

**`no event sources are configured`** — the pass refused rather than acting. An
empty allow-list makes every subscription an orphan, and reconciliation would
correctly delete all of them. The cause is almost always migrations 0043/0045
not applied, or the connection not going through the system functions.

**`include_descendants must be true for SharedDrive subscriptions`** — the field
is required AND immutable, so a subscription created without it cannot be fixed,
only replaced. `Reconciler.request` sets it for every Drive source.

**`adopted a subscription we had no record of`** (warning, not an error) — a
previous pass created a subscription and died before writing it down. Adoption
is what stops the next pass creating a second one on the same target and
double-delivering everything. Worth reading the surrounding log for why the
earlier pass died.

**`deleted a stray subscription`** (warning) — a subscription notifying our
topic whose target is not allow-listed, or a second one on a target that already
had one. Only subscriptions pointing at *our* topic are ever deleted: the
service account is shared between environments, and deleting by "we do not
recognise it" alone would have staging delete production's subscriptions.

**Nothing arrives, and the subscriptions are all active.** The subscription is
only the first half. Check the delivery path in this order, because each step
fails in a way that looks like the previous one succeeding:

1. Is the endpoint reachable from outside without an Access challenge?

   ```bash
   curl -s -o /dev/null -w '%{http_code}\n' -X POST \
     https://work-staging.openbases.com/v1/google/events -d '{}'
   ```

   `302` means Cloudflare Access is intercepting it at the edge and Google's
   push never reaches us. The path-scoped bypass application
   (`cloudflare_zero_trust_access_application.pubsub_push`) is not applied —
   check `tofu state list | grep pubsub_push`. `401` is correct: the request
   reached the receiver and was refused for having no token.

2. Is the receiver registered at all?

   ```bash
   scripts/on.sh staging control "journalctl -u control-api --since today -o cat \
     | grep 'push endpoint not registered'"
   ```

   Any match means `WG_PUBSUB_PUSH_AUDIENCE` did not reach the process. Note
   that an unregistered route and a registered one both answer 401 — they differ
   only in the body, which the receiver leaves empty. `make infra-check` now
   fails when a deployed variable is not read by any Go file, which is how this
   was missed for a week.

3. Are deliveries arriving but failing?

   ```bash
   scripts/on.sh staging control "journalctl -u control-api --since today -o cat \
     | grep 'refused a push delivery'"
   ```

**A source stuck in `failed`** — read `last_error`:

```bash
scripts/on.sh staging control "sudo -u postgres psql -X -d workgraph -c \
  'SELECT display_name, state, last_error, last_renewed_at FROM system_event_sources() s JOIN event_subscriptions ON true LIMIT 20'"
```

`last_renewed_at` never advances on a failure, so it answers "when did this last
actually work".

## Changing which event types we listen for

Edit `internal/workspace/sources.json` and deploy. The next pass patches each
subscription in place — the API's patch method accepts `event_types`, so the
subscription id survives and no window exists in which nothing is subscribed.

Every entry in that file was established one at a time with `validateOnly`,
because the documented list contains types a shared-drive target rejects. One
wrong entry is not a degraded subscription: Google refuses the whole create, so
the source silently never subscribes. That happened on the first deployment —
`permission.v3.updated` does not exist, the verified name is
`permission.v3.edited` — which is why the file is embedded and there is only one
copy of the list.

## Adding a source

A migration, not an API call or a dashboard click. `db/migrations/0045_seed_event_sources.sql`
is the pattern: kind, external id, display name, visibility and a written
rationale — all five required, because a source whose classification nobody
chose is how client-confidential material ends up in a summary the whole company
can read. Then deploy; the next pass subscribes.

Removing one: set `enabled = false` in a migration. The next pass deletes the
subscription rather than letting it expire, because left alone it keeps
delivering for days after someone revoked access.

## The delegated identity

`WG_GOOGLE_SUBJECT` is currently a person's own Workspace address. Domain-wide
delegation cannot mint a token for the service account itself — Drive and Meet
resources belong to users — so the reconciler has to act as somebody.

That somebody should not be a person. Every subscription and every read is
attributed to them, their leaving the company breaks discovery, and their own
Drive access defines what the platform can see. The replacement is a dedicated
Workspace account with no mailbox, added to the three shared drives as a viewer,
and set as the subject. Until then this is a known deviation, not a design.

## What this does not do

It does not copy anything. Subscriptions carry a reference to a file or a
conference, the receipt records what was claimed, and the work re-reads the
source at the moment of use (ADR-0027). A delivery is not evidence of its own
contents: a verified push token proves the message came from Pub/Sub, not that
what it says is true.
