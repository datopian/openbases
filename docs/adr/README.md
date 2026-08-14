# Architecture decision records

**Epic:** `wg-8yv` · **Work package:** `wg-8yv.2`

One file per decision, named `NNNN-short-slug.md`. A decision is recorded **before** the code that
depends on it, and is linked to the Bead that motivated it.

## Status values

| Status | Meaning |
|---|---|
| `proposed` | Written, not yet accepted. |
| `accepted` | In force. Implementation must comply. |
| `superseded` | Replaced. The record stays; it names its successor. |
| `deprecated` | No longer applies and has no successor. |

A decision is never edited into a different decision. Change means a new ADR that supersedes the
old one, exactly as knowledge records are superseded rather than overwritten.

## Index

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-shared-company-graph.md) | Shared company graph, many role-aware views | accepted |
| [0002](0002-execution-cells-by-trust-boundary.md) | Execution cells by trust boundary | accepted |
| [0003](0003-beads-as-canonical-work-state.md) | Beads as canonical work state | accepted |
| [0004](0004-postgresql-for-control-plane.md) | PostgreSQL for identity, links, approvals, audit, projections | accepted |
| [0005](0005-gastown-behind-an-adapter.md) | Gas Town behind an adapter | accepted |
| [0006](0006-cloudflare-zero-trust-ingress.md) | Cloudflare zero-trust ingress | accepted |
| [0007](0007-github-app-authentication.md) | GitHub App authentication | accepted |
| [0008](0008-typed-agentd-api.md) | Typed `agentd` API, no generic shell | accepted |
| [0009](0009-approval-digest-binding.md) | Approval digest and protected actions | accepted |
| [0010](0010-no-kubernetes-in-v1.md) | No Kubernetes in version 1 | accepted |
| [0011](0011-knowledge-layers.md) | Capture, evidence, candidate, durable-knowledge layers | accepted |
| [0012](0012-workspace-for-capture-git-for-durable-state.md) | Google Workspace for capture, Beads/Git for durable state | accepted |
| [0013](0013-source-acl-inheritance.md) | Source ACL inheritance and protected sanitisation | accepted |
| [0014](0014-governed-self-improvement.md) | Governed self-improvement through evaluation and pull requests | accepted |
| [0015](0015-gitops-immutable-production.md) | GitOps and immutable production, no undocumented drift | accepted |
| [0016](0016-interim-unenforced-main-protection.md) | `main` protection unenforceable, with compensating controls | accepted |
| [0017](0017-not-adopting-paperclip.md) | Not adopting Paperclip as the execution and governance layer | accepted |
