# ADR-0003: Beads as canonical work state

- **Status:** accepted
- **Date:** 2026-08-10
- **Plan reference:** §6.4, §7.1, §7.2

## Context

Work state could live in PostgreSQL, where the rest of the control plane lives. But agents already
operate through Beads: dependencies, readiness, hierarchy, and agent memory are native to it, and
Gas Town dispatches against it. Duplicating that model in PostgreSQL would create two trackers that
drift, and drift in a work tracker means agents act on stale readiness.

## Decision

Beads/Dolt is the canonical store for work items, hierarchy, and dependencies. All writes go
through `bd` commands; the control plane never mutates Dolt tables directly. Reads prefer
`bd ... --json`; read-only SQL is allowed only for reporting queries that the CLI cannot express
efficiently, and stays behind the adapter with version-specific tests.

Beads is used **without forking**. Business semantics are expressed through issue type, labels,
descriptions, and relations — never custom columns. `bd ready` remains the authoritative answer for
unblocked work inside a graph.

## Consequences

- No schema fork, so upstream upgrades stay possible.
- PostgreSQL holds a *projection* of work state for ranking and filtering. The projection is
  rebuildable and must never quietly become a second tracker.
- Every `bd` invocation is recorded with command, actor, cell, database, version, duration, and
  result, so failures can be reconciled.
- The label taxonomy becomes load-bearing and is versioned in `beads-bootstrap/labels.yaml`.

## Alternatives considered

**PostgreSQL as canonical, Beads as a mirror.** Rejected: agents would read a mirror that can lag,
and readiness computed in two places will disagree.

**Fork Beads to add columns.** Rejected explicitly by the plan's non-goals. It ends upstream
compatibility for a gain that labels already provide.
