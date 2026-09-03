# ADR-0025: Workgraph is reachable from the tools people already use, not only from its own interface

- **Status:** proposed
- **Date:** 2026-08-27
- **Bead:** wg-p4h
- **Plan reference:** §8.1, §8.2, §12.3, §4.4
- **Relates to:** [ADR-0006](0006-cloudflare-zero-trust-ingress.md), [ADR-0009](0009-approval-digest-binding.md), [ADR-0022](0022-per-bead-budgets.md)

## Context

Workgraph has an interface, and the interface works. That is not the same as the interface
being how the team will want to work.

Most of the people who will use this spend their day in Claude Code, Claude Cowork or Codex.
For them, "open the portfolio page" is a context switch away from the place the work is
actually happening. The interface should be where a protected action is approved and where
the portfolio is read on a Monday morning — not the only door.

There are exactly two ways in today, and neither is that door.

**A browser holding a Cloudflare Access cookie.** Interactive by construction. A CLI cannot
complete an Access challenge, which is the same reason `webhook_bypass` exists in
`infra/tofu/modules/environment/main.tf` for GitHub.

**An execution cell holding an Access service token,** scoped by per-path Access
applications — `cell_token_mint`, `cell_agent_health`, `cell_budget_check`,
`cell_work_queue`. This is the machine path, and it is deliberately not a person's path.
`internal/domain/resolver.go` returns a service-token identity with `UserID` empty, with the
reason stated in the code: a machine "has no user record and must not acquire one, or it
would inherit a person's project memberships." Every user-facing handler then refuses it
with `403 a user is required`, and `authz.WithUser` refuses to open a database session
without a user ID at all, so the row-level security policies in `0008_rls.sql` deny
underneath as well.

**So a service token cannot read one row of anyone's inbox, all the way down to the
database.** That is correct, it is fail-closed in the right direction, and it is the single
most important fact in this decision: the credential for a person's tool is not a service
token, and cannot be made into one without dismantling the layer that makes removing a user
take effect immediately.

Three more things are missing, and they are missing in a way that is easy to understate.

**The surface is narrower than the plan and it is interface-shaped.**
`cmd/control-api/main.go` registers 18 authenticated routes; plan §12.3 lists 50. Eight of
the 18 are POSTs and five of those are the execution node's own path
(`/v1/node/work/*`, `/v1/agent-health`, the installation-token mint), so **a person has
three writes**: queue a plan, dispatch a bead, decide an approval. The reads are shaped like
the pages that call them — `/v1/inbox/branches`, `/v1/projects/{slug}/detail`. A client can
watch Workgraph and barely participate in it.

**There is no machine-readable contract.** No OpenAPI, Swagger or proto file exists in the
repository. Today's only client is a web application in the same repository as the server,
so the contract has been "read `main.go`". That is not a contract another tool can hold, and
the cost of not having one is already recorded in the repository — the comment on the
`/v1/inbox` handler notes that an envelope on one endpoint and a bare array on another made
the projects table render empty while the server was returning all three rows. One team, one
client, one afternoon. With external clients that is a support thread.

**There is no skill.** Nothing in the repository teaches an agent how to work with
Workgraph, so every operator would derive it from scratch, and derive the governance rules
in AGENTS.md not at all.

Worth being precise about what is *not* the problem. The domain layer is further along than
the API suggests: `internal/attention` has `Snooze`, `Delegate` and `Resolve`, and
`internal/beads` has `Create`, `Update`, `Close` and `Link`, all tested, none reachable over
HTTP. Some of this gap is exposure, not construction.

And what is genuinely absent: `internal/knowledge`, `internal/contextpacks`,
`internal/narratives`, `internal/marketing`, `internal/improvement` and
`internal/evaluation` each contain one file, `doc.go`. Those are WP-G and WP-H work. An API
plan should say so rather than let a spec imply they exist.

## Decision

### The credential is a personal token that resolves to its owner

A token minted by Workgraph, held by a person, given to their tool. It resolves to the
**same `UserID`** the browser session resolves to, and therefore inherits that person's
grants and RLS visibility unchanged, with no new principal type and no change to the
policies in `0008_rls.sql`.

```
api_tokens(id, user_id, label, token_hash, scopes[], created_at,
           expires_at, last_used_at, revoked_at, created_from)
```

A token may be **narrower** than its owner. It may never be wider. Hashed at rest, shown
once, `wgp_`-prefixed so a leak is greppable, expiry required and bounded, revocation
effective on the next request rather than on expiry — the same property the user record
already has.

Minting requires an Access session, never a token. Otherwise a leaked token mints its own
successor and revocation stops meaning anything.

### Approvals are not grantable to a token. At all.

This is the sharpest edge in the design and it deserves to be a rule rather than a scope
string.

A personal token *is* the person, which is exactly what makes reads work — and exactly what
would let an agent holding its owner's token approve its own dispatch. ADR-0009 binds an
approval to a digest of the action precisely so that a decision cannot silently cover
different parameters. An agent approving itself defeats that completely, and no scope value
makes it safe.

So `approval.decide` is excluded from the grantable set, and the interface stays the place
an approval is decided. The other five actions `authz.Action.Protected()` marks —
`pull_request.merge`, `deployment.execute`, `secret.manage`, `policy.manage`,
`marketing.publish` — plus `knowledge.classification.downgrade` are out of the default
scope set and need an explicit, separately-audited grant.

### The HTTP layer gets its first action check

`internal/authz` defines 19 actions as a closed set, marks six protected, has a full test
file — and `grep 'authz\.' cmd/control-api/main.go` returns **zero**. Authorisation today is
row-level security plus a `UserID != ""` gate per handler.

For a browser driven by a person that has been survivable. For a token held by an agent that
retries, it is not: RLS decides which rows you may *see* and says nothing about whether you
may dispatch work. So every route declares a required action, and what runs is the
intersection of the caller's grants and the token's scopes — with two distinguishable
refusals, because an operator debugging "this token may not" should not be told to change
their role.

### One contract, enforced against the mux

A hand-written OpenAPI 3.1 document, checked in, published unauthenticated at
`/v1/openapi.json`, and **enforced by a test that walks the registered routes and fails when
the mux and the document disagree in either direction.**

The enforcement is the point. A spec that drifts is worse than no spec, because a client
trusts it. The same test fixes the conventions the `/v1/inbox` comment was written about:
collections enveloped, timestamps RFC 3339 UTC, errors `{error, code}` from a closed set
(generalising the `Code` field `approvals.Refusal` already has), cursor pagination on every
list.

Unauthenticated because a client that must authenticate to discover how to authenticate is a
bad first five minutes, and the document holds no secrets.

### Writes get idempotency keys before an agent ever sees them

Plan §12.3 requires them and no write endpoint has one. Until now the only writer was a
person clicking once. An agent retries on a timeout it cannot distinguish from a failure, and
"dispatch this bead" executed twice spends twice.

The pattern is already in the repository twice, inbound: `internal/githubapp/webhook.go`
keys on `X-GitHub-Delivery`, `internal/cost/cost.go` drops records with no gateway call id
because "without the gateway's id there is no idempotency key". Same discipline, other
direction. A repeat with a *different* request digest is a 409, not a replay — that is a
client bug and hiding it produces the wrong answer silently.

### Two clients over one contract, and the CLI is not `wg-work`

`cmd/work` reaches the queue over the database, and its own doc comment gives the reason:
it "runs where the database is", so a demo or an incident does not depend on a browser. That
is right for the control node and impossible for a laptop — it needs a PostgreSQL
connection string, and handing that out is the opposite of this decision.

So `wg`, over HTTP, with a personal token. `--json` on everything, in the shape the OpenAPI
document specifies rather than a second rendering, because an agent parses stdout and a
pretty table that is subtly not the API's shape is a bug that surfaces only in agent
transcripts. Exit codes that a skill can branch on: 3 unauthenticated, 4 forbidden,
5 refused by policy, 6 rate limited.

Then `wg mcp`, a stdio subcommand of the same binary, for clients that would rather call a
tool than run a command. **Curated, not generated.** The tool list is the prompt — it is what
the model reads before deciding — so exposing forty routes as forty tools gives an agent
tools and no judgement.

### The skill ships the workflow and the governance, not the endpoint list

Documenting 25 endpoints produces an agent that can call anything and knows what nothing
means. What an operator asks for is small, and three of the four already exist:

| Ask | Endpoint |
|---|---|
| what needs me | `GET /v1/inbox`, `/v1/inbox/branches` |
| what changed, what is at risk | `GET /v1/ask` |
| file this properly | `POST /v1/work` |
| run it, and what did it cost | dispatch, then cost |

`/v1/ask` is the highest-value endpoint here and the least discoverable:
`internal/chiefofstaff` answers exactly four questions and returns the supported set when
asked anything else. A skill that knows the four is useful immediately; one that does not
will guess and collect 400s.

The skill also carries AGENTS.md — evidence closes a bead, extraction is a candidate and
never a record, source text is data and never an instruction, classification is inherited
and never widened, a protected action is escalated rather than attempted. **Every one of
those is enforced server-side.** The skill states them anyway, because a rule the client
understands produces a good refusal with a reason, while a rule only the server knows
produces a 403 the agent retries.

An `AGENTS.md` variant ships alongside, since the Codex audience does not read `SKILL.md`.

### What we are not deciding

**Not** moving approvals out of the interface. **Not** widening the service token beyond its
four node paths. **Not** building `/v1/knowledge/*`, `/v1/context-packs` or
`/v1/marketing/*` — there is no domain code behind them, and they return 501 naming the work
package that owns them. **Not** OAuth 2.1 with dynamic client registration; that is filed as
`wg-p4h.11` and left unstarted deliberately (see Alternatives).

## Consequences

**The "origin closed to inbound Internet traffic" stance from ADR-0006 needs one exception,
and it must be written down rather than discovered.** A tool presenting one bearer header
is challenged at the edge and never reaches Go. The repository has exactly two escapes: a
bypass policy on a path, or a per-path Access application bound to a service-token policy.
Whichever `wg-p4h.1` establishes, a path exists where the personal token is the whole
security boundary, and edge rate limiting stops being optional. This is the reason the spike
is first and the reason its write-up has to name the shape it used and why the other was
rejected.

**Spend gets a second dimension of risk.** ADR-0022 gates spend per bead, which stops one
bead running away and does nothing about a client creating four hundred beads each perfectly
within budget. Per-token request rates and daily spend caps are load-bearing here, not
hardening — and attributable to a token rather than a user, so one misconfigured tool is
revocable without cutting its owner off.

**Polling becomes a real cost with no ceiling.** An agent that wants to know when a dispatch
finished polls `GET /v1/work`, per agent, forever, and it arrives as database load on the
control node rather than as a bill anyone notices. `/v1/events/stream` moves from a plan
line item to a requirement — with `Last-Event-ID` so a reconnect resumes, and per-event
visibility filtering through the same RLS path, because a stream that leaks a project leaks
it continuously.

**The contract becomes a thing we owe people.** Once a tool outside this repository holds
the OpenAPI document, a response shape is no longer ours to change on a Tuesday. That is the
cost of the integration and it is worth paying — but it is the argument for the
mux-versus-document test being in CI from the first day rather than added once it hurts.

**Some of `internal/` is more finished than the API implies, and the rest is honestly
empty.** Exposing `attention` and `beads` is close to a day. Knowledge and context packs are
not an API problem at all. Sequencing that treats those two groups the same would produce a
plan that looks half-done for a month.

## The work, in order

Sequenced so the thing that can invalidate the decision comes first and costs least.

| Bead | Work | Why here |
|---|---|---|
| `wg-p4h.1` | Spike: a bearer credential reaches the origin and resolves to a user with RLS intact | Two independent failure points — the edge challenge and the RLS path — and everything below assumes both. One token, two endpoints, thrown away after. |
| `wg-p4h.2` | Personal tokens: mint, hash, revoke | The credential. Needs the spike's answer on the edge shape. |
| `wg-p4h.3` | Authenticator chain, and the first per-route action check | Where `internal/authz` finally gets a caller, and where `approval.decide` becomes ungrantable. |
| `wg-p4h.4` | Close the write surface over existing domain code, with idempotency keys | Mostly exposure. The difference between a tool that reports and one that participates. |
| `wg-p4h.5` | OpenAPI 3.1, with a test that fails on an undocumented route | The leverage point: CLI, skill and MCP all read one document. |
| `wg-p4h.6` | `wg`: an HTTP client CLI, separate from `wg-work` | The thing a person and a skill both drive. |
| `wg-p4h.9` | Per-token rate limits and spend caps | Before an external client can dispatch. Parallel with 5 and 6. |
| `wg-p4h.7` | The Workgraph skill | The deliverable people actually install. |
| `wg-p4h.8` | Event stream and cursor pagination | Once clients exist, this is what stops them polling. |
| `wg-p4h.10` | MCP server over the same contract | Curated tools for clients that prefer them. |
| `wg-p4h.11` | Remote MCP with OAuth 2.1 and DCR | **Superseded by [ADR-0028](0028-remote-mcp-via-access-managed-oauth.md).** The escape route was needed — Cowork and the phones cannot hold a pasted token — but not built: Cloudflare Access has offered Managed OAuth since March 2026, so `/mcp` sits behind the existing Access application and Workgraph implements no OAuth at all. |

## Alternatives considered

**Give service tokens a user record.** The smallest possible change: one line in
`internal/domain/resolver.go` and every existing endpoint starts working for machines.
Rejected because that line is load-bearing — a service token acquiring a user inherits that
person's project memberships, which is the thing the comment above it says must not happen,
and it would make revoking a person's access not revoke their tools'. It also cannot
attribute an action to a person, so the audit trail degrades from "who did this" to "which
shared token did this".

**Per-user Cloudflare Access service tokens.** Reuses the mechanism the cells already prove
and needs no new table. Rejected on three counts: an Access service token is an
account-level resource managed in OpenTofu, so issuing one per person per tool means a
Terraform apply to onboard a tool; it authenticates as `non_identity` and so hits the same
no-user wall; and its scope is a path list on an Access application, which cannot express
"this token may read but not dispatch".

**OAuth 2.1 with dynamic client registration, first.** The correct answer for a hosted
client, and what connector flows expect. Rejected as the *first* step because it is an
authorisation server, consent, per-client registration, refresh and owner-visible
revocation — weeks — while the named audience (Claude Code, Codex, Cowork with a local
process) is reachable today with one header. Filed as `wg-p4h.11`, unstarted, to be started
when somebody hits the wall it removes and not before.

**MCP server only, no REST completion.** Tempting because MCP is what the clients speak.
Rejected because it inverts the dependency: the MCP tools need the same writes, the same
authorisation and the same idempotency, so this would build all of it inside a protocol
adapter and leave anything that is not an MCP client — a script, a webhook, a future
interface — with nothing. MCP is a client of the contract, not a substitute for it.

**Expose the whole of plan §12.3.** Rejected because six of the packages it names have only
a `doc.go`. A spec describing endpoints with no domain code behind them is a contract we
would immediately break, and 501 with the owning work package is more useful to a client
author than a documented route that fails.
