# ADR-0029: A Jev acceptance gate that verifies delivered work

- **Status:** proposed
- **Date:** 2026-09-28
- **Bead:** sa-2o9
- **Supersedes:** —
- **Superseded by:** —

## Context

Workgraph *asserts* that work is done rather than *verifying* it. An agent closes
its own bead, and the platform's honest signals stop at the mechanical: `system_bead_outcome`
distinguishes `done` (closed **and** a pull request exists) from `closed_unlanded`
(closed with nothing in the repository), but neither says whether the change that
landed actually satisfies what the bead asked for. The `workgraph_beads_file` tool
warns about the gap directly — without acceptance criteria "an agent invents its own
criteria and closes against those."

The cost of the gap is real and recent. `datc-yft` closed with a detailed comment
claiming schema, migration and crypto were "implemented and verified," 12/12 tests
green — and delivered nothing to the repository (its landing failed silently; see
ADR-context in the landing `--no-verify` fix). Even after landing works, the deeper
question is unanswered: *did the delivered change meet the acceptance criteria?*
Today that question has three unsatisfying answers — trust the agent, pay an LLM to
re-read the diff, or wait for a human. The first is how `datc-yft` happened; the
second is slow and expensive enough that we do not do it; the third does not scale
to autonomous delivery.

Two things changed that make a fourth answer cheap. `work_refs.acceptance` is now
projected into the control plane (a bead's acceptance criteria are readable without
touching the node). And TypeSafe AI's **Jev** model is on the Cloudflare AI Gateway
as `typesafe/jev` — our existing routing, tagging, unified-billing and per-cell
spend-cap substrate. Jev is a non-autoregressive "System One" model: it evaluates a
single `state` against many typed questions in parallel and returns **calibrated
values with confidence**, not text — `Noul` (truth, 0–1), `Choice`, and `Score`. It
costs $0.042 per 1M input tokens and $0 output, with a 32k context and zero data
retention. That is cheap enough to run on every delivery, and its output is a number
we can threshold on rather than prose we have to parse.

## Decision

Add an **acceptance gate**: when a bead is closed or landed, evaluate the delivered
change against that bead's own acceptance criteria with Jev, and record a calibrated
verdict on the bead.

- A new `internal/jev` client calls `typesafe/jev` through the Cloudflare AI Gateway,
  reusing the gateway auth and unified-billing path the runner already uses (the
  `internal/geminiproxy` precedent), so the call is tagged, metered and budget-governed
  like every other model. It sends one `state` and many typed questions and returns
  typed answers with confidence.
- A new `internal/verify` builds the questions for one bead — one `Noul` per
  acceptance criterion ("does the delivered change satisfy this criterion?") plus an
  overall `Score` — from the bead's `acceptance` (projected in the bead-body change)
  and a summary of the landed diff and the agent's closing comment. It returns a
  structured verdict: per-criterion confidence, an overall score, and pass/fail at a
  configurable threshold.
- The verdict is recorded on the bead (a `verification` object beside `outcome` in
  `system_bead_detail`, surfaced through `/v1/work/{bead}` and `workgraph_bead`).

**Shadow first.** The gate ships recording-only: it computes and stores the verdict
but does **not** change `outcome` and does **not** block a close or a landing. We run
it against real beads, calibrate the confidence thresholds, and only then, in a
later decision, let a failed verification hold a bead out of `done`. The gate never
becomes ground truth: it is a judgment with a confidence number, so enforcement pairs
a threshold with a human fallback, and the objective checks a repository already has
(`check_command`, which runs the build and tests) stay the gate for *correctness* —
Jev covers the *semantic* "does this meet what was asked" that tests cannot.

## Consequences

Makes easy: an automated, per-criterion answer to "did we actually deliver this?" on
every bead, at a fraction of a cent; a calibrated signal that can later gate `done`,
route a PR to auto-merge or to a human, or reopen a bead with the exact criterion it
failed. It is the missing half of the delivery loop — the platform already plans,
dispatches and lands; this checks.

Makes hard / forecloses: nothing structural. It adds a dependency on a third-party
early-access model, kept behind our gateway and feature-flagged, with the human path
intact. The 32k context means a large diff is judged against a changed-files/diff
summary plus the acceptance text, not the whole patch — so the verdict is only as
good as the summary it is given, which the shadow period exists to calibrate.

## Alternatives considered

- **An LLM re-reads the diff.** Works, but slow and expensive enough that we do not do
  it in practice, and it returns prose we must parse into a decision. Jev returns a
  calibrated number for ~1% of the cost, which is what makes "on every bead" viable.
- **Rely on `check_command` alone.** Build-and-test proves the code is correct, not
  that it is the code that was asked for — a green build on the wrong feature still
  passes. The two are complementary and both are kept.
- **Trust the agent's self-close (status quo).** This is the behaviour `datc-yft`
  exposed; it cannot be the basis for autonomous delivery.
- **Enforce from day one.** Rejected: the thresholds are unknown until measured against
  real beads, and a mis-calibrated gate that blocks good work is worse than the gap it
  closes. Shadow mode measures first.
