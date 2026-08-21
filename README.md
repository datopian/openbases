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
