# ADR-0018: Tiered model routing, with frontier models as an escalation path

- **Status:** accepted
- **Date:** 2026-08-16
- **Bead:** wg-2a0
- **Plan reference:** §4.7, §13.2, §14.8
- **Relates to:** [ADR-0005](0005-gastown-behind-an-adapter.md), [ADR-0009](0009-approval-digest-binding.md), [ADR-0017](0017-not-adopting-paperclip.md)

## Context

Every agent call currently goes to Anthropic, and the default Gas Town cost tier put every role on
Opus. One day of testing produced a $23 bill of which $13 was Mayor, Deacon, witness and refinery
polling — patrol roles cost more than the work they supervised. Tiering to Haiku and Sonnet cut the
rate; it did nothing about volume, because the problem was 1,616 requests doing no work.

That experience produces two separate lessons, and conflating them wastes the second:

1. **Idle inference is the first cost problem.** Solved by not running agents when there is no work
   (`scripts/dispatch_bead.sh`, the reaper timer). No model choice beats zero requests.
2. **Model choice is the second.** Once agents only run when there is work, the question becomes
   which model should do which work, and the current answer — whichever model the harness defaults
   to — is not a decision anybody made.

Phase H makes this urgent. Meeting extraction, commitment detection, classification, summarisation,
attention ranking, duplicate detection and staleness checks will be the overwhelming majority of
inference volume, and none of it needs a frontier model.

Deciding this now rather than later matters because model choice would otherwise settle inside Gas
Town role configuration and individual agent profiles, where changing it later means touching every
one of them.

## Decision

### The ordering principle

Cost work happens in this order, and the order is the decision:

1. **Zero unnecessary calls.** Event-driven instead of polling, deduplication, stop conditions,
   idle detection, backoff.
2. **Reduce context.** Compact context packs, cached stable prefixes, no replaying history.
3. **Cheapest model that reliably succeeds.**
4. **Escalate only when that fails.**
5. **Hard spend and rate limits as the final safety net, not the first control.**

A ten-times cheaper model still wastes money if an idle loop keeps firing, which is exactly what
happened: 1,616 requests doing no work. Model choice is step three, and reaching for it first is
the mistake this ordering exists to prevent.

### Deterministic code before inference

A large amount of what looks like agent work is not a judgement at all. Do not ask a model whether
a polecat is alive — check the process. Whether CI passed — read the check state. Whether a bead
changed — compare versions. Whether something is overdue — compare timestamps.

Inference is for ambiguity: given these failures and this activity, is the agent stuck or
productively investigating? Everything decidable by comparison should be decided by comparison, and
this applies throughout Workgraph, not only to the orchestrator.

### Tiers

Model selection is a property of the WORK, not of whichever agent picks it up. Four tiers:

| Tier | Default | Used for |
|---|---|---|
| T0 mechanical | Llama 3.3 70B Instruct FP8 Fast | classification, routing, tagging, extraction, short operational text |
| T1 reasoning | Gemma 4 | briefs, planning, decomposition, review, routine writing |
| T2 coding | Kimi K2.7 Code | non-trivial implementation, debugging, repo-scale change |
| T3 expert | Claude Sonnet, then Opus | architecture, ambiguous failure, critical review |

Opus is disabled by default and requires explicit human approval. Sonnet requires a cheaper tier to
have been attempted first, unless the work is marked critical.

**T0 must not be a reasoning model, and this was measured rather than assumed.** The plan originally
named GLM-4.7-Flash here. Classifying fifteen real bead titles into bug/task/chore through the
staging oss gateway:

| Model | max_tokens | Correct | Blank | Neurons | Time |
|---|---|---|---|---|---|
| llama-3.3-70b-instruct-fp8-fast | 64 | **13/15** | 0 | **35.6** | **11.7s** |
| gemma-4-26b-a4b-it | 400 | 9/15 | 2 | 110.8 | 58.7s |
| gemma-4-26b-a4b-it | 256 | 3/15 | 11 | 111.6 | 56.9s |
| glm-4.7-flash | 1200 | 5/6 | — | 105.8 | 38.9s |
| glm-4.7-flash | 24 | 0/6 | 6 | 6.6 | 6.2s |

The structural finding matters more than the ranking. A reasoning model spends its token budget
thinking before it answers, so at a T0-sized budget it returns **nothing at all** — GLM at 24 tokens
and Gemma at 256 both consume the budget without producing content. Give it room to finish and it
costs three times the neurons and four to five times the latency, to do work that has one right
answer and needs no deliberation.

This is a statement about the TIER, not about which model is better in general: Gemma 4 remains the
right T1 default, because T1 is briefs and review, where deliberation is the product.

**Any T0 client must handle `content: null`.** GLM-4.7-Flash returns its answer in `reasoning` with
`content` null, so a client reading only `content` sees an empty string and no error — and silently
classifies everything as blank. That is a client requirement regardless of which model is default,
because the default can change.

Scope of the claim: n=15 on one task. Enough to pick a default and to rule out reasoning models at
this tier; not enough to treat 13/15 as a precise accuracy figure. Re-measure per task type as
Phase H lands.

A bead carries its own budget and escalation path, so the work determines the model:

```yaml
model_class: normal
max_cost_usd: 1.00
escalation: [gemma-4, kimi-k2.7-code, claude-sonnet]
```

Cost is recorded per bead — planned against actual, broken down by model — so routing can later be
tuned on measured quality per dollar for Datopian's own work rather than on published benchmarks.

### Two inference planes

The architecture has two planes, and forcing one abstraction across both was the error in the first
draft of this decision.

**Control-plane inference** — meeting extraction, classification, attention ranking, summaries,
memory candidates, marketing signals, context preparation. We write these callers, so requests go
through dynamic routes with metadata and get conditionals, budget nodes and fallback. Fine-grained.

**Execution inference** — Mayor, Witness, Polecats, Refinery, Deacon. An external harness controls
these calls. Coarse role assignment and gateway spend limits.

They share cost telemetry, and nothing else.

### Optimise workers aggressively, control loops experimentally

Gas Town is NOT Claude-only, and an earlier draft of this ADR said otherwise. Its runtime system
supports Claude, Codex, Gemini, Cursor, OpenCode, Copilot and custom CLI agents, and `role_agents`
can assign different runtimes per role. Verified directly: `gt config agent set probe-gemma "gemini
--approval-mode yolo"` is accepted, and the cost tier already creates `claude-haiku` and
`claude-sonnet` aliases the same way.

What remains true is narrower and still matters: some integration is Claude Code specific — `gt
signal` is documented as a Claude Code hook handler — so a different runtime may lose hook-driven
behaviour.

The operational objection outweighs the technical possibility. A bad extraction is recoverable. A
bad orchestrator creates cascading work: retries, incorrect status, duplicate dispatch, bad merges.
So worker intelligence is optimised aggressively, and control-loop intelligence experimentally,
behind shadow evaluation before any role changes.

### Budget exhaustion is an operational event, not an error

A 429 from a spend limit must not produce a retry storm. Exhaustion pauses the affected work and
raises an attention item — "the coding allocation for this project is exhausted, three beads
queued" — rather than retrying into the limit. For control-plane inference a dynamic route falls
back to a cheaper model instead; for the harness, the limit is a circuit breaker and the correct
response is to stop and tell somebody.

## The constraint that shapes the implementation

**Dynamic routes cannot be used by the agent harness.** Cloudflare's dynamic routing is reachable
only through the OpenAI-compatible `/compat/chat/completions` endpoint, and its documentation states
plainly that dynamic routing is not available on the REST API. Gas Town spawns Claude Code, which
speaks Anthropic's `/v1/messages`. A Claude Code agent therefore cannot address `dynamic/<route>` at
all.

This splits the implementation in two, and the split is not a compromise — the two paths have
genuinely different requirements:

**Workgraph-authored inference** — the control plane, Phase H extraction, classification,
summarisation, attention ranking. We write these callers, so they use dynamic routes with metadata
(project, task type, risk, bead) and get conditional routing, budget nodes and fallback for free.
This is where the volume is, so it is where tiering pays.

**Agent harness inference** — Gas Town polecats, witness, refinery, Mayor, Deacon. These are Claude
Code processes and must use provider-native endpoints. They are tiered by ROLE through
`gt config cost-tier` and bounded by per-gateway spend limits, which is coarser but sufficient:
patrol roles are already on Haiku, workers on Sonnet.

Moving agents onto cheaper non-Anthropic models would mean either replacing Claude Code as the
harness — Gas Town integrates with its hook system, so this is not a configuration change — or
running a translating proxy that accepts `/v1/messages` and speaks `/compat/chat/completions`. The
proxy is the smaller change and is recorded as follow-up work, not adopted here, because a proxy in
the inference path is a new failure mode and the measured saving from patrol tiering has not yet
been observed over a full week.

## Consequences

Context assembly becomes a cost control, not just a quality one. A cheap coordinator builds a
context pack and an expensive model consumes it; the expensive model should not be the thing that
discovers context. Prompts are structured so stable material stays stable, because Kimi's cached
input is roughly a fifth of its uncached input price and a regenerated system prompt forfeits that.

Cost becomes a first-class object in the interface — spend by model, and cost per completed bead, PR
review, meeting processed — rather than a provider bill discovered at month end.

Two credentials are needed that we do not have: the deploy token cannot read the Workers AI model
catalogue, and a gateway token scoped to `AI Gateway Run` cannot reach Workers AI models, which
require `Workers AI Read`.

## Alternatives considered

**Keep everything on Anthropic and rely on spend limits.** Limits stop runaway spend; they do not
reduce it. A budget that blocks work when exhausted converts a cost problem into an availability
problem.

**Move the agent harness to a cheap model immediately.** Rejected for now, though it is a
configuration change rather than a rewrite. Patrol roles do not go quiet when a weaker model
struggles — they retry, which increases request count, and a witness that misreports agent health
corrupts orchestration rather than merely costing money. The place to prove a cheap model is Phase H
extraction, where the failure mode is a poor suggestion in a review inbox. Control-loop roles change
only after a shadow evaluation.

**Route everything through a translating proxy so agents can use dynamic routes.** Deferred. It is
the right long-term answer if agent inference stays significant, and the wrong first step while the
larger saving is available with no new component.
