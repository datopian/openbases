# ADR-0001: Shared company graph, many role-aware views

- **Status:** accepted
- **Date:** 2026-08-10
- **Bead:** wg-8yv.2
- **Plan reference:** §1.2, §2.3

## Context

Two obvious topologies both fail.

A single shared, omniscient Gas Town "Mayor" that every employee talks to makes Gas Town the
security and identity boundary for a multi-user company product. It is not built for that: one
compromised or over-broad session can read every client's work.

A fully independent Gas Town per employee, with nothing shared but GitHub, fragments context.
Nobody can answer "what is happening across the company", work is duplicated, and attention cannot
be routed because there is no shared notion of what needs a decision.

## Decision

Operate one shared company control plane and company-level work graph, with project-specific Beads
graphs for detailed work and personal private graphs for individual reminders. Employees interact
through role-aware agents and views that query only the data they are authorised to see.
Relationships that cross Beads databases live in the control plane link layer.

## Consequences

- Company-wide attention routing and portfolio views become possible, because there is one place
  that knows about all authorised work.
- Authorisation becomes the load-bearing component. Every query filters at the database or domain
  service layer; a missed filter is a client data leak, so row-level security backs the application
  checks.
- Cross-graph relationships need explicit modelling in `work_links` rather than a native Beads
  reference. This is extra work, but it makes the edges queryable and auditable.
- Gas Town remains an execution engine, not an identity system.

## Alternatives considered

**One shared Mayor.** Rejected: Gas Town is not a multi-tenant security boundary, and the blast
radius of a single leaked context is the whole company.

**Per-employee towns.** Rejected: no shared state means no portfolio view, no attention routing,
and duplicated work — the specific failure the product exists to fix.
