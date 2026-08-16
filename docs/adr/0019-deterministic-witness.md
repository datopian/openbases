# ADR-0019: A deterministic witness, with the model reserved for the one ambiguous case

- **Status:** accepted
- **Date:** 2026-08-17
- **Bead:** wg-0ft
- **Plan reference:** §4.7, §7.6, §13.1
- **Relates to:** [ADR-0005](0005-gastown-behind-an-adapter.md), [ADR-0018](0018-tiered-model-routing.md)

## Context

ADR-0018 established the ordering: **zero unnecessary calls before cheaper calls**. This is the
first application of it, and it is the one with the largest measured saving available.

Gas Town's Witness is a per-rig agent that, in its own documentation, does four things:

> - Detects stalled polecats (crashed or stuck mid-work)
> - Nudges unresponsive sessions back to life
> - Cleans up zombie polecats (finished but failed to exit)
> - Nukes sandboxes when polecats complete via `gt done`

None of those is a judgement. All four are conditions over state that Gas Town already computes, in
Go, and publishes:

- `gt polecat list <rig> --json` returns a per-polecat `verdict` — `SAFE_TO_NUKE`,
  `NEEDS_MQ_SUBMIT` or `NEEDS_RECOVERY` — alongside the booleans behind it and a `reason` such as
  `git-dirty` or `git-unpushed`.
- `gt polecat check-recovery` exists specifically, per its help text, to be "used by the Witness to
  determine appropriate cleanup action".
- Agents write `.runtime/heartbeats/<prefix>-<name>.json` with a timestamp and a state.
- The daemon and the polecats write typed events to `events/witness/*.event`, including
  `POLECAT_DONE <name> exit=COMPLETED`.

The Witness is nevertheless a Claude Code session, primed with a multi-thousand-token prose formula
(`mol-witness-patrol.formula.toml`) that instructs a model to run those commands and interpret their
output, re-sent every `scan_interval_seconds` — 300 by default — for as long as the town is up. One
day of testing produced 1,616 requests and 365,671 output tokens across the patrol roles: more than
the work they were watching, and generated whether or not any work existed.

Paying a frontier model to read a boolean is the cost problem in its purest form.

## Decision

**Disable the stock Witness patrol and replace it with a program.**

1. The town's `settings/config.json` gets `"disabled_patrols": ["witness", "handler"]`. This is Gas
   Town's own documented town-settings schema; the daemon logs `Witness patrol disabled in config,
   skipping` and kills a leftover session, so the two cannot both run.
2. `cmd/witness` runs on each execution node as the cell user. It wakes on changes to the witness
   event channel, with a slow safety sweep behind it, reads `gt polecat list --json` and the
   heartbeat files, and applies a state machine (`internal/witness`).
3. Routine outcomes are settled in the program. A completed polecat is nuked with
   `gt polecat nuke`; a working session is left alone.
4. Anything that could **lose work** is escalated to a person through the attention inbox, never
   automated: uncommitted or unpushed work, and work that was never submitted to the merge queue.
   Gas Town's own documentation says to escalate exactly these.
5. The **one** genuinely ambiguous case — a live session that has gone silent, where "stuck" and
   "thinking hard" are indistinguishable from outside — is the only case a model could legitimately
   judge. It is currently routed to the inbox too, so the witness makes **zero** inference calls.

### What is deliberately not changed

- **The refinery patrol stays on.** It is already event-driven upstream: the daemon logs `No pending
  refinery events and no session running for <rig>, skipping spawn`. It costs only when there is a
  merge to process, which is the behaviour being aimed at everywhere else.
- **The Deacon stays on.** Disabling the town-level watchdog is plausible but untested, and removing
  the last supervisor deserves its own evidence rather than being bundled here.
- **The Mayor is not addressed.** `disabled_patrols` accepts `mayor` and has no effect — the Mayor
  is not a patrol and the daemon starts it regardless. It runs on `sonnet[1m]` and is pure overhead
  for this deployment, which has its own chief of staff (WP-F3). Filed as wg-8yv.51.
- **Gas Town is not forked.** Every fact read is a documented Gas Town output; every action taken is
  a Gas Town command. This replaces a prompt with a program, not an orchestrator with another
  orchestrator (ADR-0005).

## Consequences

**The witness now makes no model calls at all.** That is a measurement, not a claim: every decision
including the quiet ones is written to `agent_health_events` with a `used_model` column, so a pass
of forty observations and one escalation is visible as such.

**Escalation now means something.** `attention_items` had no writer — the inbox was empty by design
with nothing populating it. The witness is its first producer, which also exercises the WP-F2 path
end to end.

**An escalation that reaches nobody is reported as a failure.** A project with no lead, no backup
operator and no organisation admin, or a repository absent from the registry, produces an
`unaddressed` count and an error-level log rather than a silent success. This is the failure mode
that would otherwise look identical to "nothing was wrong".

**A standing problem produces one inbox item.** Escalations are keyed on rig, polecat and reason, so
repeated observation refreshes the item rather than duplicating it — and an operator's snooze
survives the machine seeing the problem again, because that is their decision, not the machine's.

**Nuking is the only destructive action and it is doubly gated.** It requires Gas Town's
`SAFE_TO_NUKE` verdict *and* evidence that the polecat did work (a branch or a merge request),
because `gt polecat nuke` destroys the agent bead and its CV chain — nuking an idle member of a
polecat pool would throw away work history to reclaim nothing. A property test asserts that no
combination of inputs produces a nuke while anything is at risk.

**The stall threshold is a tuning risk, not a correctness one.** Twelve minutes is generous on
purpose: a polecat running a test suite is routinely quiet for several minutes, and a false stall
interrupts real work and re-primes a context. Too long merely delays an inbox item.

**Gas Town upgrades can break this.** The witness depends on the shape of `gt polecat list --json`,
the heartbeat files and the `disabled_patrols` key. That dependency is the same class as everything
else in `versions.lock`, and the compatibility fixture is where a change must be caught; the parser
ignores unknown fields so an addition upstream is not a breakage.

## Alternatives considered

**Lengthen the patrol interval.** `scan_interval_seconds` is a formula variable and could be raised
from 300 to, say, 1800. It reduces the call rate by six and changes nothing structural: the cost is
still proportional to how long the town is up rather than to how much work there is, and the
decisions are still made by a model reading booleans.

**Put the Witness on a cheap model.** Already done — `role_agents` puts it on Haiku at low effort
(ADR-0018). It cut the rate and left 1,616 requests in place. This is precisely the step-2 move that
ADR-0018 says must not precede step 1.

**Do nothing, because the town is only up during a dispatch.** `scripts/dispatch_bead.sh` already
bounds the exposure, and that was the single largest saving. But it caps the damage rather than
removing it: the patrol still burns the whole time a real task runs, which is exactly when the node
is busiest and least able to spare it.

**Let the witness escalate to a cheap model instead of a person.** Tempting for the silent-session
case, and the design leaves room for it. It is not done now because a model asked "is this agent
stuck?" from outside the session has no more evidence than the program does — it would be inference
performing confidence rather than adding information.
