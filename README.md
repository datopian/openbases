# Datopian Workgraph

A shared, permissioned company control plane: one durable work graph, a company-wide attention
layer, and controlled execution by humans and agents.

- **Work state** lives in Beads/Dolt.
- **Software execution** happens in isolated Gas Town execution cells.
- **Durable knowledge** lives in reviewed Beads and Git-native documents.
- **Code, infrastructure, policy, and reviewed knowledge** live in GitHub, and change through
  pull requests.

The implementation specification is the approved baseline v1.1 in
[`datopian/company-workgraph`](https://github.com/datopian/company-workgraph) under `plan/`.
Decisions that shape this codebase are recorded in [`docs/adr/`](docs/adr/).

## How it fits together

Two machines, neither with an inbound port. Everything a person touches goes through Cloudflare
Access first; everything an agent touches is scoped to one security domain.

```mermaid
flowchart TB
    person["👤 People<br/><small>executives, leads, operators, clients</small>"]

    subgraph cf["Cloudflare"]
        access["<b>Access</b><br/><small>Google Workspace IdP, MFA<br/>one audience per application</small>"]
        tunnel["<b>Tunnel</b><br/><small>nodes dial out; no inbound port</small>"]
        aigw["<b>AI Gateway</b> ×3<br/><small>oss · internal · client<br/>spend limits, per-role metadata</small>"]
        r2[("<b>R2</b><br/><small>state, backups,<br/>evidence, audit</small>")]
    end

    subgraph control["Control node — no agent code runs here"]
        api["<b>control-api</b> (Go)<br/><small>127.0.0.1:8080<br/>serves the embedded React UI + /v1</small>"]
        pg[("<b>PostgreSQL</b><br/><small>row-level security;<br/>the app role is not the owner</small>")]
        hq[("<b>Beads HQ</b><br/><small>company work graph (Dolt)</small>")]
        recon["<b>reconcile</b><br/><small>timer; replays missed events</small>"]
    end

    subgraph exec["Execution node — untrusted agent code"]
        direction TB
        subgraph cell1["cell: oss"]
            gt1["<b>Gas Town</b><br/><small>town → rig → polecats</small>"]
            ag1["🤖 <b>agents</b><br/><small>Claude Code sessions</small>"]
            bd1[("Beads<br/><small>(Dolt)</small>")]
        end
        subgraph cell2["cell: client-nged"]
            gt2["<b>Gas Town</b>"]
            ag2["🤖 <b>agents</b>"]
            bd2[("Beads")]
        end
        wit["<b>wg-witness</b><br/><small>deterministic health monitor<br/>makes no model calls</small>"]
    end

    gh["<b>GitHub</b><br/><small>App, webhooks, pull requests</small>"]
    models["<b>Model providers</b><br/><small>Anthropic · Workers AI</small>"]

    person -->|"browser"| access
    access -->|"signed JWT"| tunnel
    tunnel --> api
    api --- pg
    api --- hq
    recon --- pg

    gh -.->|"webhook, HMAC-signed<br/><small>GitHub cannot pass Access</small>"| api
    api -.->|"short-lived token,<br/>one repository"| gh

    ag1 & ag2 -->|"push, open PR"| gh
    ag1 & ag2 -->|"inference"| aigw
    aigw --> models
    gt1 --- bd1
    gt2 --- bd2
    gt1 --- ag1
    gt2 --- ag2

    wit -->|"escalations"| api
    exec -.->|"service token,<br/>path-scoped"| api
    control --- r2
    exec --- tunnel

    classDef boundary fill:#f6f8fa,stroke:#57606a,color:#1f2328
    classDef danger fill:#fff8f0,stroke:#bc4c00,color:#1f2328
    class control,cf boundary
    class exec,cell1,cell2 danger
```

The dashed edges are the ones that matter for security:

- **A cell never holds a git credential.** It asks `control-api` for a token scoped to one
  repository, over an Access application bound to that single path, and gets one that expires
  within the hour. The GitHub App private key can mint tokens for *every* installed repository, so
  it stays on the control node where no agent code runs.
- **GitHub webhooks bypass Access**, because GitHub cannot complete an Access challenge. An
  HMAC signature is the only thing standing between that endpoint and anyone who learns its URL.
- **Cells cannot see each other.** Separate Linux users, `0700` homes, a `0711` cells root so one
  cannot even enumerate the others' names, cgroup limits, and `hidepid` so one cannot read
  another's process arguments — where a gateway token would otherwise be visible.

## How one unit of work moves

```mermaid
flowchart LR
    bead["<b>1. Bead</b><br/><small>work item in the graph</small>"]
    sling["<b>2. dispatch</b><br/><small>town up, bead slung<br/>to a polecat</small>"]
    agent["<b>3. 🤖 agent</b><br/><small>reads, edits, commits<br/>in its own worktree</small>"]
    pr["<b>4. pull request</b><br/><small>scoped token,<br/>merge queue</small>"]
    proj["<b>5. projection</b><br/><small>webhook → PR and CI state<br/>in the database</small>"]
    ui["<b>6. interface</b><br/><small>portfolio, project,<br/>attention inbox</small>"]
    human["👤 <b>decision</b><br/><small>approve, delegate,<br/>snooze, escalate</small>"]

    bead --> sling --> agent --> pr --> proj --> ui --> human
    human -.->|"closes or reshapes"| bead
    agent -.->|"work at risk,<br/>or a stalled session"| ui

    classDef step fill:#f6f8fa,stroke:#57606a,color:#1f2328
    class bead,sling,agent,pr,proj,ui,human step
```

The town comes up for a dispatch and is torn down after it, in a trap rather than on the happy
path. Patrol agents that poll cost money for as long as they are running, whether or not there is
work — on one day of testing they made more requests than the work they were supervising. The
health monitor that replaced the largest of them makes no model calls at all.

Nothing on the return path approves itself. An agent can open a pull request; it cannot merge one,
widen a classification, or decide its own budget.

## Which model does which work

Model choice is a property of the **work**, not of whichever agent picks it up. A classification
and an architecture review are not the same job and should not cost the same, and the default —
whichever model the harness happens to reach for — is not a decision anybody made. The reasoning is
[ADR-0018](docs/adr/0018-tiered-model-routing.md); this is what it means in practice.

### What actually runs today

Two models do all of the platform's work:

| Model | What it runs |
|---|---|
| `claude-sonnet-5` | workers — `polecat` and `crew`, the agents that do the work |
| `claude-haiku-4-5` | patrol — `mayor`, `deacon`, `witness`, `refinery`, `dog`, `boot` |

**No open-weight model serves production traffic.** Of 3,356 recorded calls,
195 went to open-weight models — all of them on 16–17 August, tagged
`t0-bakeoff` and `probe`, and all of it the measurement below. Nothing has used
one since, because the only planned caller for T0–T2 is control-plane inference,
and that is not built (`wg-sn4`).

Opus does not appear in any role map and the platform never selects it. The Opus
spend in the gateway logs is untagged, which is what a developer's own Claude
Code session looks like — not the agents.

The tiers below are a **decision, not a description**. They say what should run
what once there is something to route.

### Four tiers (designed; T0–T2 have no caller yet)

| Tier | Default model | Work it is for |
|---|---|---|
| **T0** mechanical | `@cf/meta/llama-3.3-70b-instruct-fp8-fast` | classification, routing, tagging, extraction, short operational text |
| **T1** reasoning | `@cf/google/gemma-4-26b-a4b-it` | briefs, planning, decomposition, review, routine writing |
| **T2** coding | Kimi K2.7 Code | non-trivial implementation, debugging, repo-scale change |
| **T3** expert | `claude-sonnet-5`, then `claude-opus-5` | architecture, ambiguous failure, critical review |

Opus is off by default and needs explicit human approval. Sonnet needs a cheaper tier to have been
tried first, unless the work is marked critical.

**Switching the agent tier to a non-Anthropic model is not a configuration
change**, and it is worth knowing why before planning around it. Every agent
call goes to `…/workgraph-<env>-<domain>/anthropic` — the gateway's Anthropic
path — because Claude Code speaks Anthropic's `/v1/messages`. Point it elsewhere
and the wire format no longer matches. The two ways out are replacing Claude
Code as the harness, which Gas Town's hook integration makes more than a setting,
or a proxy that accepts `/v1/messages` and speaks `/compat/chat/completions`.
The proxy is the smaller change and is tracked as `wg-b7z`. Until one exists,
`--model` on `wg-runner` can select any Anthropic model and nothing else.

**T0 must not be a reasoning model, and that was measured rather than assumed.** The plan
originally named GLM-4.7-Flash here. Classifying fifteen real bead titles into bug/task/chore
through the staging `oss` gateway:

| Model | max_tokens | Correct | Blank | Neurons | Time |
|---|---|---|---|---|---|
| `llama-3.3-70b-instruct-fp8-fast` | 64 | **13/15** | 0 | **35.6** | **11.7s** |
| `gemma-4-26b-a4b-it` | 400 | 9/15 | 2 | 110.8 | 58.7s |
| `gemma-4-26b-a4b-it` | 256 | 3/15 | 11 | 111.6 | 56.9s |
| `glm-4.7-flash` | 1200 | 5/6 | — | 105.8 | 38.9s |
| `glm-4.7-flash` | 24 | 0/6 | 6 | 6.6 | 6.2s |

The structural finding matters more than the ranking. A reasoning model spends its budget thinking
before it answers, so at a T0-sized budget it returns **nothing at all** — GLM at 24 tokens and
Gemma at 256 both consume the budget without producing content. Give it room to finish and it costs
three times the neurons and four to five times the latency to do work that has one right answer.
That is a statement about the *tier*, not about which model is better: Gemma 4 stays the right T1
default, because T1 is briefs and review, where deliberation is the product.

Two consequences for anyone writing a caller. **Any T0 client must handle `content: null`** —
GLM-4.7-Flash returns its answer in `reasoning` with `content` empty, so a client reading only
`content` sees a blank string and no error, and silently classifies everything as blank. And the
scope of the measurement is n=15 on one task: enough to pick a default and to rule out reasoning
models at this tier, not enough to treat 13/15 as an accuracy figure.

### The work carries its own tier

A bead is meant to declare what it needs and how far it may escalate, so the *work* selects the
model rather than the agent that happens to pick it up:

```yaml
model_class: normal
max_cost_usd: 1.00
escalation: [gemma-4, kimi-k2.7-code, claude-sonnet]
```

*Designed, not built — the schema records cost per bead today, but nothing reads an escalation path
yet (`wg-fpt`).* What exists now is the per-bead **budget**, which is enforced: `wg-budget` refuses
a dispatch before an agent starts, resolving most-specific-first — the bead's own ceiling, else its
project's, else its cell's.

### Two inference planes, tiered differently

The split is forced by a real constraint, not by taste. Cloudflare's dynamic routing is reachable
only through the OpenAI-compatible `/compat/chat/completions` endpoint and is unavailable on the
REST API. Gas Town spawns Claude Code, which speaks Anthropic's `/v1/messages`, so **an agent
cannot address a dynamic route at all.**

**Control-plane inference** — extraction, classification, attention ranking, summarisation, context
preparation. We write these callers, so they can carry metadata (project, task type, risk, bead)
and get conditional routing, budget nodes and fallback. This is where the volume will be, so it is
where tiering pays. *Not built yet — tracked as `wg-sn4`.*

**Agent inference** — Mayor, Deacon, Witness, Refinery, polecats, crew. Claude Code processes on
provider-native endpoints, so they are tiered by **role** instead, which is coarser and sufficient:

| Role | Model | Effort | Why |
|---|---|---|---|
| `polecat` | `claude-sonnet` | medium | does the actual work |
| `crew` | `claude-sonnet` | medium | does the actual work |
| `mayor` | `claude-haiku` | low | supervises nothing here — Workgraph does that job itself |
| `deacon` | `claude-haiku` | low | town watchdog |
| `witness` | `claude-haiku` | low | mostly replaced by `cmd/witness`, which makes no model calls |
| `refinery` | `claude-haiku` | low | event-driven; costs only when there is a merge |
| `dog`, `boot` | `claude-haiku` | low | patrol |

The full map lives in [`infra/ansible/roles/gastown/defaults/main.yml`](infra/ansible/roles/gastown/defaults/main.yml).
[`internal/runner/plan.go`](internal/runner/plan.go) carries the same tiers for the two roles the
direct runner starts — `polecat` and `crew` — under the names the Claude Code CLI accepts, which
are not the names Gas Town uses. Same tier, different spelling; see the note below.

The map is owned in Ansible rather than by `gt config cost-tier`, because two owners of one map
fight: once any entry differs from the preset, `gt` reports the tier as `custom` rather than `budget`, so a task
that re-applies the tier whenever it is not `budget` fires on every run and undoes the difference.
That was observed, not theorised.

**Reasoning effort matters more than the model did.** Output was 89% of tokens and effectively all
of the cost in the first bill, so every patrol role runs at `low`. One spelling note that cost real
time: the CLI flag is `--effort`, not `--reasoning-effort`, and the CLI model name is `sonnet`, not
`claude-sonnet` — given the latter it prints a warning to stderr and then **runs anyway on a
default**, so the wrong tier fails quietly rather than loudly.

### What it actually costs

Tiering is step three, not step one. The order is: zero unnecessary calls, then less context, then
the cheapest model that reliably succeeds, then escalation, and spend limits last as a net rather
than a control. One day of testing produced a $23 bill of which $13 was patrol roles polling —
1,616 requests doing no work. No model choice beats not making the call, which is why the town comes
up for a dispatch and is torn down after it, and why the health monitor that replaced the largest
patrol makes no model calls at all.

Every call is recorded per model, imported from the gateways into `usage_records`. From staging, all
3,356 records to date:

| Model | Calls | Spend | What produced it |
|---|---:|---:|---|
| `claude-opus-5` | 376 | $23.60 | untagged — developer sessions, not the platform |
| `claude-haiku-4-5` | 2,236 | $16.40 | patrol roles |
| `claude-sonnet-5` | 503 | $11.97 | workers |
| open-weight, 8 models | 195 | $0.01 | the T0 bake-off above, 16–17 Aug |

Two things to read out of it, and one thing not to.

Haiku took four times Sonnet's call volume for less than one and a half times its
cost, which is patrol tiering doing its job — that is the part that is working.
The Opus row is the largest single line and none of it is the platform: it is
untagged, and untagged means no role, cell or bead header, which is what a
developer's own Claude Code session looks like. Untagged spend is not a rounding
error to leave alone; it is 45% of this table.

What *not* to read is the open-weight row as evidence of tiering. Those 195 calls
are the bake-off, ten days old and never repeated. Cheap tiers cost a cent here
because almost nothing has used them.

Spend is attributed by role, cell, project and **bead**, so `wg-budget` can refuse a dispatch
before an agent starts rather than after it has spent the money. Three gateways — `oss`, `internal`,
`client` — divide one shared ceiling rather than each repeating it, because writing the whole figure
to each made the real ceiling three times the agreed one.

## Getting started

```bash
make bootstrap    # install the pinned gt/bd/dolt matrix (checksum-verified) and dependencies
export PATH="$PWD/.toolchain/bin:$PATH"
make check        # everything CI runs: fmt, vet, race tests, versions, migrations
make build        # build control-api, worker, and agentd into ./bin
```

`make bootstrap` downloads only release artefacts and verifies each SHA-256 against
[`versions.lock`](versions.lock). A mismatch aborts the install. Nothing resolves to `latest`.

## Layout

```
apps/web/          React + TypeScript user interface
cmd/               control-api, worker, agentd
internal/          domain logic and provider adapters
db/migrations/     forward-only SQL migrations
infra/             OpenTofu, Ansible, Cloudflare, Google integration resources
deploy/            Compose and systemd definitions
formulas/          Gas Town workflow formulas
policies/          policy bundles evaluated by the approval engine
evaluations/       golden and regression suites
docs/adr/          architecture decision records
docs/runbooks/     operator runbooks
docs/go-live/      the go-live evidence pack
test/              contract, integration, and end-to-end suites
versions.lock      the validated gt/bd/dolt compatibility matrix
```

## How work happens here

Read [`AGENTS.md`](AGENTS.md) before making a change. The short version:

- Every change is attached to a Bead. There is no Markdown TODO list.
- One Bead, one short-lived branch `bead/<id>-<slug>`, one pull request.
- `main` is protected; direct pushes fail, for humans and agents alike.
- Staging and production are deployment targets, never development environments. Every persistent
  change is represented in Git.
- Never commit a secret. Never upgrade `gt`, `bd`, or `dolt` independently. Never publish
  unreviewed model extraction. Never widen a classification. Nothing approves itself.

## Current state

Phase A — repository and governance bootstrap — is complete: repositories, ADRs, policies,
formulas, the pinned toolchain, the core schema, CI gates, and a building service and web
skeleton.

Phases B to I are tracked as Beads. Services intentionally fail closed where they are unfinished:
the production authenticator denies every request, `agentd` refuses jobs without a signed
capability, and `/health/ready` reports not-ready rather than claiming a health it cannot verify.
