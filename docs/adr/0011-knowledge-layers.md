# ADR-0011: Capture, evidence, candidate, and durable-knowledge layers

- **Status:** accepted
- **Date:** 2026-08-10
- **Bead:** wg-8yv.2
- **Plan reference:** §14.1, §14.3, §14.4

## Context

The tempting design is to index everything into a vector store and let agents retrieve. That
produces a system where nobody can say what the company actually decided, where a model's
paraphrase is indistinguishable from a commitment someone made, and where a deleted or re-permissioned
source leaves derived claims circulating with no lineage.

## Decision

Separate four layers and never collapse them:

1. **Capture** — Google Meet, Docs, GitHub, agent runs, deployments, metrics. The provider stays
   canonical for raw human-authored content.
2. **Evidence** — immutable source identity, revision, author, timestamp, permissions, checksum,
   and an encrypted snapshot in R2.
3. **Candidates** — typed, machine-extracted statements with source spans and confidence. Never
   durable memory.
4. **Durable state** — accepted Beads, reviewed Markdown, ADRs, policies, and playbooks.

A human review step sits between layers 3 and 4, always.

## Consequences

- Every durable statement can be traced to a source span and a named reviewer.
- Extraction can be re-run against the evidence snapshot when a prompt improves, and the result
  compared against what humans accepted — which is what makes the evaluation suite possible.
- Review is real work, and the review inbox must be good enough that people use it. A bad review UI
  makes the whole model fail by starvation.
- The vector index is a rebuildable projection, never a source of truth.

## Alternatives considered

**Index everything and retrieve.** Rejected: no provenance, no review, no supersession, and no
answer to "who decided this".

**Only store human-written notes.** Rejected: it loses the commitments and decisions that are made
verbally and never written down, which is most of them.
