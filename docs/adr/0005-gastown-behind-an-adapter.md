# ADR-0005: Gas Town behind an adapter

- **Status:** accepted
- **Date:** 2026-08-10
- **Plan reference:** §7.5, §7.6

## Context

Gas Town is moving quickly, and Gas City already separates reusable orchestration primitives from
Gas Town's opinionated layout. If product code shells out to `gt` directly, every CLI change
becomes a cross-cutting refactor, and switching orchestrators means rewriting the product.

## Decision

All orchestration goes through the `Orchestrator` interface in `internal/gastown`: health, list
agents, dispatch, pause, resume, nudge, activity, and convoy state. Nothing outside that package
invokes `gt`. The `gt` binary is pinned by exact version and checksum in `versions.lock`.

## Consequences

- A future move to Gas City or another orchestrator changes one package.
- The interface is a natural place to enforce invariants that must not be lost: dispatch already
  refuses a request without a signed capability or a complete work-reference tuple, before the
  adapter is even implemented.
- Some Gas Town capabilities will not fit the interface. Adding a method is a deliberate decision,
  which is the point.
- Contract tests run against the pinned binary so a CLI change fails CI rather than production.

## Alternatives considered

**Call `gt` directly from handlers.** Rejected: it spreads shell invocation and output parsing
across the codebase and couples the product model to a fast-moving CLI.
