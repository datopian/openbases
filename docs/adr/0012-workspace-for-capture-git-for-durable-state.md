# ADR-0012: Google Workspace for capture, Beads and Git for durable state

- **Status:** accepted
- **Date:** 2026-08-10
- **Bead:** wg-8yv.2
- **Plan reference:** §14.1, §14.2, §20

## Context

Agents prefer Markdown in Git. People prefer meeting where they already meet and drafting where
they already draft. Forcing collaborative drafting into pull requests to suit agents would reduce
capture to whatever survives the friction — which in practice is very little.

## Decision

Keep Google Workspace as the low-friction capture and collaboration surface. Workgraph converts
**explicitly selected** source material into reviewed operational state. The pilot ingests exactly
one registered recurring Meet and one allow-listed Drive source. No domain crawl, no personal
drives, no mirroring of every document.

Meet uses Workspace Events subscriptions for conference, transcript, and smart-note artefacts, with
retrieval through the Meet REST API. Drive uses Workspace Events only where the feature is enabled
and production-approved, and otherwise the stable `changes.watch` and `changes.list` reconciliation
path — because Drive Workspace Events was still Developer Preview at the time of the plan.

## Consequences

- Adoption does not depend on changing how people work.
- Subscription lifecycle becomes critical infrastructure: renewal, expiry alerting, deduplication,
  and catch-up after an outage are all required, not optional.
- Two Drive paths must be maintained until the event path is production-approved. The adapter
  boundary keeps the choice in one place.
- Sources not on the allow-list are dropped without retry. That is correct behaviour, not an error,
  and the connector logs it as such.

## Alternatives considered

**Mirror all of Drive into Git.** Rejected: a permission leak with extra steps, and it makes Git a
document store it was never meant to be.

**Manual copy-paste of meeting outcomes.** Rejected: it is what happens today, and it is the thing
that fails.
