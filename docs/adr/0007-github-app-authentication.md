# ADR-0007: GitHub App authentication

- **Status:** accepted
- **Date:** 2026-08-10
- **Bead:** wg-8yv.2
- **Plan reference:** §9.1, §9.2, §17.1

## Context

Agents need to clone, push, and open pull requests. A personal access token would be long-lived,
broadly scoped, hard to attribute, and catastrophic if it leaked into a log or a commit — it would
carry the permissions of a human across every repository they can reach.

## Decision

Use a Datopian GitHub App installed on selected repositories. `agentd` mints a repository-scoped
installation token per job, injects it into an isolated temporary agent home, configures `gh` and
Git HTTPS without writing it into remotes, removes it when the process ends, redacts it from
telemetry, and never stores it in Beads.

Requested permissions are the minimum: metadata read; contents, pull requests, and issues read and
write; checks, actions, and commit statuses read; deployments write only where used. Repository
administration is not requested.

## Consequences

- A leaked token expires quickly and reaches only the repositories of one job.
- Actions are attributable to the app installation and the dispatching actor.
- Token minting becomes a hot path that must be reliable and rate-limit aware.
- Secret-redaction tests are mandatory, and WP-E3 acceptance requires proving no token appears in
  git config, remote URLs, logs, or the work item.

## Alternatives considered

**Shared machine-user PAT.** Rejected: long-lived, over-scoped, and unattributable.

**Per-developer tokens.** Rejected: agent actions would run with a human's full authority.
