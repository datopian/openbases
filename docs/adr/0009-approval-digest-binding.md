# ADR-0009: Approval digest and protected actions

- **Status:** accepted
- **Date:** 2026-08-10
- **Plan reference:** §10.1, §10.2

## Context

"Anu approved the deploy" is not sufficient. If approval attaches to an intent rather than an
artefact, the artefact can change between approval and execution — a rebased diff, a re-planned
Terraform run, a rebuilt image tag — and the approval silently covers something nobody reviewed.

## Decision

Every approval binds to a digest of the exact proposed action: the diff, the plan artefact, the
image digest, or the serialised parameters. If any of it changes, the approval is invalid and a new
one is required. Approval records are append-only and carry the requester, work item, project, risk
level, required approver roles, evidence, expiry, decision, actor, reason, timestamp, and execution
result.

Self-approval is forbidden. High-risk classes require two approvers.

## Consequences

- A rebased pull request or a re-planned apply needs re-approval. This is friction by design.
- Digest computation must be canonical and stable, or approvals will be invalidated by
  insignificant reordering. The digest inputs are defined per action class and tested.
- WP-F2 acceptance explicitly requires proving that a changed action invalidates its approval.

## Alternatives considered

**Approve the work item, not the artefact.** Rejected: it is exactly the gap that lets an
unreviewed change execute under a valid-looking approval.

**Time-boxed approval without binding.** Rejected: time does not constrain *what* executes.
