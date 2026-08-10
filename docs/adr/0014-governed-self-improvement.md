# ADR-0014: Governed self-improvement through evaluation and pull requests

- **Status:** accepted
- **Date:** 2026-08-10
- **Plan reference:** §14.8, §16.6

## Context

The system will accumulate exactly the data needed to improve itself: run outcomes, human
corrections, failures, and costs. Acting on that automatically is how a system quietly rewrites its
own guardrails. Not acting on it wastes the most valuable signal the pilot produces.

## Decision

Self-improvement is a governed pipeline: evidence → Bead → pull request with tests and evaluation
cases → CI → staging replay against the regression suite → human approval proportionate to risk →
immutable production rollout → before/after measurement, with automatic rollback criteria.

The system may propose changes to code, prompts, formulas, skills, policies, tests, and runbooks.
It may not approve or deploy its own protected change. Security, authentication, authorisation,
approval policy, deployment logic, audit behaviour, secrets, and data classification always require
a qualified human reviewer; high-risk changes require two.

Human accept, edit, and reject actions become structured evaluation data. Improvement starts with
prompts, schemas, retrieval rules, examples, and deterministic checks. Fine-tuning is out of scope
until a clean, sufficiently large correction dataset exists.

## Consequences

- Every behaviour change is reviewable, replayable, and reversible.
- Building the evaluation suite is a prerequisite, not a follow-up: without it, "improvement" is
  unmeasured change.
- Improvement is slower than letting a model adjust its own prompt. That is the trade.
- Registering Workgraph as a project inside itself means its own backlog is subject to the same
  approval rules — including the rule that it cannot approve its own protected change.

## Alternatives considered

**Automatic prompt tuning in production.** Rejected: unmeasured, unreviewable, unrollbackable, and
capable of relaxing a guardrail without anyone noticing.

**No self-improvement.** Rejected: it discards the correction data that is the pilot's main output.
