# ADR-0024: Multi-provider agent runtimes, with the runtime chosen per run

- **Status:** accepted
- **Date:** 2026-08-27
- **Bead:** wg-uhj
- **Plan reference:** §4.7, §13.2, §14.8
- **Supersedes in part:** [ADR-0018](0018-tiered-model-routing.md) — the "two inference planes" split stands;
  its conclusion that a translating proxy is the smallest change does not.
- **Relates to:** [ADR-0005](0005-gastown-behind-an-adapter.md), [ADR-0023](0023-direct-agent-runner.md)

## Context

Workgraph runs on Anthropic models and nothing else. That is measurable rather than
impressionistic: of 3,356 recorded gateway calls, 195 went to open-weight models, all of
them on 16–17 August, all tagged `t0-bakeoff` or `probe`. Nothing has used one since.
`claude-sonnet-5` runs the workers and `claude-haiku-4-5` runs the patrol roles. The T0,
T1 and T2 tiers ADR-0018 defined have no caller at all.

That is a concentration risk, a cost floor, and a capability ceiling. It means we cannot
put a coding model against a coding task, cannot fall back when one provider degrades,
and cannot take advantage of a frontier open-weight model the week it lands.

ADR-0018 diagnosed the blocker correctly: Claude Code speaks Anthropic's `/v1/messages`,
Cloudflare's dynamic routing is reachable only through `/compat/chat/completions`, so an
agent cannot address a dynamic route. It concluded that a translating proxy — accepting
`/v1/messages`, speaking `/compat` — was the smallest change.

**That conclusion is now wrong, and it is worth being precise about why.** Three things
changed after it was written.

**We built our own runner.** ADR-0023 introduced `wg-runner`, which starts an agent
directly rather than through Gas Town. The binary it executes is a string in our code
(`internal/runner/plan.go` builds `[]string{"claude", "-p", ...}`). Gas Town's hook
integration — the thing that made replacing Claude Code "not a configuration change" — is
not in that path at all. The work path and the patrol path are already separate processes,
and only the patrol path is bound to Gas Town.

**The harness landscape moved.** `gt` v1.2.0 ships built-in runtimes for `opencode`,
`codex`, `gemini`, `copilot`, `cursor`, `amp` and others, and documents
`gt config agent set <name> <command>` for anything else. OpenCode itself supports 75+
providers through the AI SDK, has **Cloudflare AI Gateway as a first-class provider**, and
— decisively — supports arbitrary per-provider request headers via `options.headers`.
OpenClaude is a second option in the same shape, driven by `CLAUDE_CODE_USE_OPENAI=1` plus
`OPENAI_BASE_URL`.

**Cloudflare shipped the billing half.** Unified Billing gives one credential and one bill
across providers, at `POST /client/v4/accounts/{account}/ai/v1/chat/completions`, with
`provider/model` naming and inference priced at pass-through plus a 5% fee on credits
purchased. Spend limits can now be scoped by **model, provider, or custom metadata**.
Custom Providers (beta) accepts any HTTPS endpoint, so a provider Cloudflare does not
support natively is a configuration entry rather than a dead end.

And the model we would most want is already there: `kimi-k3` is in the Cloudflare catalogue
as a Workers AI model, alongside `deepseek-v4-pro`, `glm-5.3-flash`, `gemini-3.6-flash` and
the Anthropic and OpenAI frontier lines.

So the proxy is no longer the smallest change. It is a component we would build, operate
and debug, sitting in the inference path, to work around a constraint that only binds one
of our two agent paths — and the harness that avoids the constraint entirely is already a
built-in option in the orchestrator we run.

## Decision

### The runtime is a property of the run, like the model

`wg-runner` gains a runtime alongside its existing role, model and effort. The role still
decides the default; a spec may override it.

```
wg-runner -role polecat -runtime opencode -model moonshotai/kimi-k3
```

Two runtimes to begin with, because two is what proves the abstraction and one does not:

| Runtime | Binary | Reaches | Used for |
|---|---|---|---|
| `claude` | `claude` | Anthropic models, via the gateway's `/anthropic` path | the default; unchanged behaviour |
| `opencode` | `opencode` | any gateway provider, via `@ai-sdk/openai-compatible` | everything else |

`claude` stays the default. This ADR does not migrate anything; it makes migration
possible and cheap to trial one bead at a time.

> **Superseded on 2026-09-07.** The default is now `opencode` on
> `workers-ai/@cf/zai-org/glm-5.3-flash`. What moved it was
> `scripts/model_bakeoff.sh`, which screens a candidate on context, tool use,
> and real work scored on the edit rather than the answer. GLM 5.3 Flash passed
> all three, as did the Sonnet 5 control, and the runs cost $0.0037 against
> $0.2732 — with every call landing in `usage_records` carrying `role`, `cell`
> and `bead`, which is the acceptance criterion this ADR set below.
>
> This is recorded here rather than only in the Ansible defaults because an ADR
> that says "claude stays the default" while the configuration says otherwise is
> the two-places-disagreeing problem the rest of this document warns about.
>
> The screens are an off-by-one and a file edit, so this is not evidence that
> GLM matches Sonnet on hard work. `-runtime claude -model
> anthropic/claude-sonnet-5` still overrides per run.

### Attribution is the acceptance criterion, not the model working

A run that reaches a model but arrives untagged is worse than a run that fails, because it
looks like success while sitting outside every budget. That is not hypothetical — it
happened when the gateway credential was moved from a settings file into the process
environment and spend arrived completely unattributed.

So the gate on any new runtime is not "did it answer" but:

1. the call appears in `usage_records`;
2. carrying `role`, `cell`, `project` and `bead`;
3. with a cost the per-bead budget can be checked against.

OpenCode's `options.headers` is what makes this reachable — the same `cf-aig-authorization`
and metadata headers we set today in `ANTHROPIC_CUSTOM_HEADERS` go in there instead.
**This is unverified and is the first thing to prove.** If headers do not survive, this
decision does not stand and the proxy comes back.

### Models are named `provider/model`, everywhere

`sonnet` becomes `anthropic/claude-sonnet-5`; the T0 default becomes
`meta/llama-3.3-70b-instruct-fp8-fast`. The runtime translates to whatever spelling its
CLI wants — the `claude` runtime already has to, since the CLI takes `sonnet` where Gas
Town says `claude-sonnet`.

One namespace or the tier table cannot be written down without ambiguity, and a model
identifier that means different things in two files is how the wrong tier ships as a
warning on stderr.

### The tier table moves out of Go and into configuration

`DefaultModels` and `DefaultEffort` are `map[string]string` literals in
`internal/runner/plan.go`. A model catalogue that changes monthly should not require a
binary release. They become a config file, deployed by Ansible alongside the role map that
already lives there, with the Go constants as the fallback when it is absent.

### Which plane gets which

ADR-0018's two planes stand, and the split is now about *how* rather than *whether*:

**Agent inference** — `wg-runner`, the work path. Gets multi-provider via the runtime
abstraction above. Direct provider addressing, not dynamic routes: the run already knows
what it is doing and a static choice is easier to attribute and to reason about.

**Patrol inference** — Mayor, Deacon, Refinery, under Gas Town. Stays on Claude Code for
now. `gt` supports other runtimes, but `gt signal` is a Claude Code hook handler, so this
is a real migration rather than a setting, and patrol roles are already on Haiku where the
saving is smallest. Revisit once the work path has run on a second provider for a month.

**Control-plane inference** — extraction, classification, ranking (`wg-sn4`). Gets dynamic
routes, at `/compat/chat/completions` with `model: dynamic/<route>`, because we write those
callers and they benefit from conditional routing and fallback. Note that the generic
"Unified API (OpenAI compat)" is marked deprecated in Cloudflare's docs while dynamic
routing still documents `/compat` — that inconsistency needs watching, and is a reason to
prefer Unified Billing's `/ai/v1/chat/completions` for anything that is not a dynamic route.

### What we are not deciding

**Not** moving patrol roles off Claude Code. **Not** making a non-Anthropic model the
default for any role. **Not** adopting dynamic routing for the agent path. **Not** turning
on Unified Billing before somebody has looked at the 5% credit fee against our actual
volume and decided it is worth one bill.

## Consequences

Tool-use fidelity becomes the thing that decides which models are usable, and it is not
predictable from benchmarks. An agent that cannot reliably call `bd` and `Edit` cannot do
the work whatever its reasoning score. This needs the same treatment the T0 choice got —
a measured bake-off on real beads, not a published leaderboard — and its result belongs in
this repository next to the T0 table.

A second runtime is a second thing to keep working. The pinned-toolchain discipline
(`versions.lock`, checksum-verified) has to cover `opencode`, or we have introduced exactly
the drift `make bootstrap` exists to prevent.

Cost comparison finally becomes possible. Spend is already recorded per model per bead;
once two providers do comparable work, "quality per dollar on Datopian's own work" stops
being a phrase in an ADR and becomes a query.

Concentration risk drops in a way that matters operationally, not just commercially: a
provider outage or a rate-limit ceiling stops being an outage for all agent work.

## The spike result

`wg-uhj` ran on 2026-08-27 and **passed**. OpenCode reached
`@cf/moonshotai/kimi-k2.7-code` through the staging `oss` gateway and the call arrived
attributed:

```
model                          role     cell  bead              rig    cents
@cf/moonshotai/kimi-k2.7-code  polecat  oss   wg-uhj-opencode   spike  1.8241
```

`options.headers` carries `cf-aig-authorization` and `cf-aig-metadata` intact, so
attribution survives a runtime change and the per-bead budget gate can read the cost.
This decision stands and `wg-b7z` — the translating proxy — is not needed.

Four things the spike found that the plan did not anticipate.

**The endpoint and naming work as documented.** `/compat/chat/completions` with
`provider/model` (`workers-ai/@cf/...`) accepts our existing gateway token and headers.
No new credential, no new gateway.

**`kimi-k3` is 403 on this account** — "Account 3218567 is not allowed to access
@cf/moonshotai/kimi-k3". Every other open-weight model tried returns 200:
`kimi-k2.7-code`, `kimi-k2.6`, `deepseek-v4-flash-0731`, `glm-5.3-flash`,
`qwen3-30b-a3b-fp8`, `gemma-4-26b-a4b-it`. Cloudflare documents prepaid credits as
granting "access to Workers AI models that otherwise require the Workers Paid plan", so
this is what Unified Billing is expected to lift. Until it is on, K3 specifically is out
of reach while the rest of the catalogue is not.

**Context window is a hard filter on which models can be workers, separately from
quality.** OpenCode's agent system prompt is roughly 19,900 tokens. A 24k-context model
cannot host it — `llama-3.3-70b-instruct-fp8-fast` fails with
"maximum context length is 24000 tokens... your prompt contains at least 19905 input
tokens" before doing any work. That model is a fine T0 classifier and an unusable
worker, and the two facts are unrelated. `wg-hvb` must screen on context before it
screens on quality.

**A large system prompt changes the cost model.** Answering "reply with exactly: OK" cost
1.82 cents, because ~20k tokens of prompt go in on every request regardless of the task.
Cheap per-token rates do not translate into cheap runs when the floor is that high. The
comparison that matters for `wg-hvb` is cost per completed bead, not per token — and
Claude Code's prompt size on the same task should be measured next to it rather than
assumed smaller.

## The work, in order

Sequenced so the thing that can invalidate the decision comes first and costs least.

| Bead | Work | Why here |
|---|---|---|
| `wg-uhj` | Spike: prove OpenCode carries the gateway attribution headers | Everything below is wasted if it does not. Half a day. |
| `wg-053` | Make the runtime a property of the run in `wg-runner` | The abstraction. Default stays `claude`; nothing migrates. |
| `wg-8e0` | Name models `provider/model` everywhere | Independent of the spike; can start now. |
| `wg-3tp` | Move the tier and role tables into deployed config | Needs the runtime and the naming. |
| `wg-ba9` | Pin the second runtime in `versions.lock` | Before anything depends on it in production. |
| `wg-hvb` | Bake off tool-use fidelity across candidate models | Decides which models are actually usable as workers. |
| `wg-hpv` | Scope gateway spend limits by provider and model | The net under the per-bead gate, once spend is multi-provider. |

`wg-b7z` — the translating proxy — stays open and unstarted. It is the fallback if
`wg-uhj` fails, and closing it before that is known would be closing the escape route.

## Alternatives considered

**The translating proxy (ADR-0018's conclusion, filed as `wg-b7z`).** Accept
`/v1/messages`, speak `/compat`, keep Claude Code everywhere. Rejected as the primary path:
it is a component in the inference path that we build and operate, to preserve a harness
choice, when a supported harness already speaks the protocol. It stays a fallback if
OpenCode cannot carry attribution headers — which is precisely what the first spike tests.

**OpenClaude instead of OpenCode.** A live project (30.8k stars) driven by
`CLAUDE_CODE_USE_OPENAI=1` and `OPENAI_BASE_URL`, which would also reach the gateway.
Rejected as the first runtime only because OpenCode is already a built-in in `gt` v1.2.0,
which keeps one runtime story across both agent paths if patrol roles migrate later.
Worth re-testing if OpenCode's tool-calling proves weak.

**Point Claude Code at Bedrock or Vertex.** Cloudflare documents both through the gateway.
Reaches more infrastructure but the same Anthropic models, so it addresses availability and
not diversity or cost. Worth doing later for failover; it is not this decision.

**Wait for Anthropic-compatible endpoints from other providers.** Some publish them. It
would need no code at all — but it makes our model choice hostage to which providers choose
to emulate a competitor's API, which is a worse position than the one we are in.
