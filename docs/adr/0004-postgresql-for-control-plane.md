# ADR-0004: PostgreSQL for identity, links, approvals, audit, and projections

- **Status:** accepted
- **Date:** 2026-08-10
- **Bead:** wg-8yv.2
- **Plan reference:** §6.2, §6.4, §12.1

## Context

Beads deliberately isolates databases and does not model identity, permissions, approvals, source
lineage, or cross-project relationships. Something must own those, and it must support
transactional integrity, row-level security, full-text and vector search, and a durable job queue.

## Decision

Use one PostgreSQL instance on the control node for identity, scoped role grants, the project
registry, cross-graph `work_links`, the domain event spine, knowledge sources and candidates,
approvals, audit, usage, and rebuildable projections including `pgvector`.

The job queue is PostgreSQL-backed using `FOR UPDATE SKIP LOCKED`. No Redis, Kafka, Elasticsearch,
or standalone graph database in version 1.

## Consequences

- One backup, one restore path, one set of transactional semantics to reason about.
- Row-level security backs application authorisation, so a forgotten `WHERE` clause is contained
  rather than fatal.
- PostgreSQL becomes a single point of failure for the control plane. Mitigated by 15-minute RPO
  through WAL archiving, tested restores, and the fact that Beads and GitHub hold work and code
  independently.
- Search quality is bounded by PostgreSQL full-text plus `pgvector`. Adequate at pilot scale; a
  dedicated search service can be added later behind the same retrieval interface.

## Alternatives considered

**Redis for the queue.** Rejected: another stateful service, another backup story, for a queue that
handles far less than PostgreSQL can.

**A dedicated graph database.** Rejected: the cross-graph edge count at pilot scale is trivial, and
recursive CTEs handle the traversals.
