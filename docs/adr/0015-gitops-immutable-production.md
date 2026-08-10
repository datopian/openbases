# ADR-0015: GitOps and immutable production, with no undocumented drift

- **Status:** accepted
- **Date:** 2026-08-10
- **Bead:** wg-8yv.2
- **Plan reference:** §16.3, §16.5, §3.3

## Context

An agent with SSH and provider credentials can fix anything directly on a host. Every such fix that
is not in Git makes the system less rebuildable, until the running state is the only description of
itself and the restore path is fiction.

## Decision

The Hetzner VMs are deployment targets, not sources of truth. The complete product and
infrastructure live in GitHub from the first commit.

Trunk-based development: `main` is protected and always releasable; direct pushes are disabled;
every change is attached to a Bead; one short-lived branch and worktree per Bead; incomplete work
sits behind feature flags rather than a long-lived branch; pull requests require review, tests, and
evidence; releases use signed tags and immutable image digests.

Production deployment references immutable digests, verifies signatures, takes a pre-deploy backup,
runs backwards-compatible migrations, deploys one service at a time, keeps the previous image, and
rolls back automatically on failed health or evaluation gates.

Bootstrap and investigation may use `ssh`, `wrangler`, `gh`, `hcloud`, and provider consoles, but
every persistent change must be represented in OpenTofu, Ansible, repository configuration, or an
idempotent committed script. Scheduled drift detection compares production against code. An
undocumented manual change creates an incident and is reverted or committed immediately. Emergency
edits require a break-glass event with a named actor, a command log, and a follow-up pull request.

## Consequences

- Staging and production can be destroyed and rebuilt from code, which is what makes the disaster
  recovery scenario real rather than aspirational.
- The convenient fix is prohibited. Drift detection makes the prohibition observable rather than
  a matter of trust.
- Bootstrap has a chicken-and-egg step: the first commit lands before branch protection exists.
  That is a one-time, recorded event; everything after it goes through a pull request.
- Secrets are the deliberate exception: they are entered through approved secret paths and never
  committed, with references, owners, and expiry tracked in the database.

## Alternatives considered

**Configure hosts by hand and document afterwards.** Rejected: documentation written after the fact
describes what someone remembers doing, and the restore path is never exercised.

**Mutable production with in-place updates.** Rejected: no reliable rollback and no way to prove
what is running.
