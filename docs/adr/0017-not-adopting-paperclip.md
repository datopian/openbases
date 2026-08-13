# ADR-0017: Not adopting Paperclip as the execution and governance layer

- **Status:** accepted
- **Date:** 2026-08-12
- **Bead:** wg-8yv.41
- **Plan reference:** §4.5, §7.4, §8.1, §14, §16.3
- **Relates to:** [ADR-0002](0002-execution-cells-by-trust-boundary.md), [ADR-0005](0005-gastown-behind-an-adapter.md), [ADR-0009](0009-approval-digest-binding.md), [ADR-0015](0015-gitops-immutable-production.md)

## Context

[`paperclipai/paperclip`](https://github.com/paperclipai/paperclip) overlaps a large part of this
plan: org chart, work items with blocker dependencies, heartbeat execution, git worktree execution
workspaces, approval gates, budgets with hard stops, routines, plugins, secrets, immutable activity
log, agent evaluations. MIT, self-hosted, ~77k stars, actively developed.

On a README reading it covers most of plan §3.1 items 1, 3, 6–13, 15, 16, 23 and 24 — most of
Phases D, E, F and part of G. Continuing to build those without evaluating it would mean rebuilding
maintained software by default rather than by decision.

A spike read the source rather than the documentation, testing three questions that determine
whether Paperclip could serve as our execution and governance layer. Full evidence in
[`docs/spikes/2026-08-12-paperclip.md`](../spikes/2026-08-12-paperclip.md).

## Decision

**Do not adopt Paperclip as the execution or governance layer.** Continue with the architecture in
this plan.

Three findings are architectural rather than cosmetic, and each conflicts with a decision already
made here.

### 1. The no-remote-git contract conflicts with GitOps

`packages/adapters/AUTHORING.md` states a hard invariant: *"No adapter may depend on a git remote
for cross-run state. Never `git push` from adapter runtime code."* It is enforced by a static CI
check, `scripts/check-no-git-push.mjs`, that fails the build on an unapproved `git push`. The local
worktree is the only cross-run persistence boundary.

Plan §4.5 and WP-E3 are pull-request-centric: a worker creates `bead/<id>-<slug>`, runs tests, opens
a PR, GitHub webhooks update the control plane, and a merge queue merges after gates pass. ADR-0015
makes that the whole deployment model. Paperclip's own README is explicit that this is out of
scope — *"Paperclip orchestrates work, not pull requests. Bring your own review process."*

Both systems also want to own git worktrees, as does Gas Town. Two worktree managers over one
repository is a real conflict, not an integration detail.

### 2. Isolation is company-scoped; ours must be client-scoped

Paperclip has genuine sandboxing: Bubblewrap spawn confinement with `--unshare-pid`, `--unshare-ipc`,
`--unshare-uts`, `--unshare-net` for network deny, and ro-bind/bind filesystem scoping. That is
better than assumed. But:

- it is **off by default** (`filesystemScope` and `networkScope` are optional);
- it is **Linux only** — the code throws on any other platform;
- it does **not** use `--unshare-user`, so every agent runs as the server's own OS user. ADR-0002
  requires a dedicated Linux user per cell;
- **secrets are company-scoped.** `company_secrets.scope` is `company` or user-owned; there is no
  per-project or per-agent scope.

The consequence is structural. To isolate a restricted client engagement you would model it as a
separate Paperclip *company*. That buys isolation at the cost of the single cross-project portfolio
view, which is the entire premise of plan §1.2 and ADR-0001. Paperclip's isolation unit is the
company; ours has to be the client engagement *inside* one company.

### 3. Approvals are not bound to what was approved

`packages/db/src/schema/approvals.ts` stores `payload jsonb` with no digest, hash, or checksum
column, and no such value appears anywhere in the approval services. Approval rows are mutated in
place (`db.update(approvals).set({ status, decidedByUserId, decidedAt })`), so a decision overwrites
rather than appends. The approvals service performs no self-approval check.

ADR-0009 requires an approval to bind to a digest of the exact artefact, and our schema enforces
append-only decisions plus a trigger refusing self-approval. Paperclip's model authorises an
approval *record*, not a specific diff, plan output, or image digest.

## Consequences

- We continue to build Phases D through G. That cost is real and now taken deliberately: heartbeat
  execution, org chart, budgets and worktree orchestration all exist upstream and are maintained.
- Datopian carries the maintenance of an execution layer that has a well-funded open-source
  alternative. This ADR should be revisited if Paperclip adds per-project secret scoping, uid
  separation, and a push/PR-capable adapter path — those three changes would remove all three
  blockers.
- Four Paperclip designs are worth adopting, and are recorded as design inputs rather than
  dependencies:
  1. **Review policies** — `anyone` / `human_only` / `not_creator`
     (`server/src/services/issue-review-policy.ts`). `human_only` is precisely the property plan
     §14.4 needs: a model may never be the reviewer. `not_creator` is a self-approval guard.
     Applies to WP-F2 and WP-H3.
  2. **Bubblewrap spawn confinement** — additive to ADR-0002 rather than a replacement. A dedicated
     Linux user per cell plus mount, pid and network namespace confinement is stronger than either
     alone. Applies to WP-E1.
  3. **Budget scoping** by company, agent, project, goal, issue, provider and model, with warning
     thresholds and hard stops that pause agents and cancel queued work. Richer than our
     `budget_limits` table. Applies to WP-E1 and WP-I1.
  4. **Cross-run persistence discipline** — treating workspace finalize failure as a run-level error
     that gates dependent work, rather than a warning to swallow. Applies to WP-E3.

## Alternatives considered

**Adopt Paperclip wholesale and drop this plan.** Rejected: it has no knowledge layer, and the
governed ingestion pipeline in plan §14 — evidence snapshots, typed candidates, human review before
publication, classification inheritance, context packs — is the distinct contribution of this
project. Code search finds six matches for "meeting" in the whole repository.

**Adopt Paperclip as the execution layer and build the knowledge layer on top.** This was the option
the spike was commissioned to test, and it is the one the three findings above rule out. It would
mean fighting the no-remote-git contract, running one Paperclip company per client to get isolation,
and adding digest binding to someone else's approval model.

**Fork Paperclip.** Rejected on the same grounds as forking Beads (ADR-0003): it ends upstream
compatibility, and the changes needed reach into the persistence model, the isolation model and the
approval model at once.
