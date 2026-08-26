# ADR-0023: Running agents directly, not through Gas Town

Status: accepted
Date: 2026-08-26
Bead: wg-8wt
Supersedes in part: [ADR-0005](0005-gastown-behind-an-adapter.md)

## Context

Gas Town cannot start an agent on a headless node.

It starts one with `tmux respawn-pane -t <session>`, which revives the process in
a pane that already exists. It never creates the session. On a machine nobody has
sat at, no sessions exist, so every agent fails — `no tmux server running` when
there is no server, and `respawn-pane: can't find pane: hq-deacon` once there is
one. Reproduced by hand against gt's own socket, so it is gt's call rather than
anything in our wrapper.

Creating the sessions in advance gets further and is not a fix. `gt up` then
reports `✓ Mayor: hq-mayor` while `pgrep claude` returns zero — the tick is not
evidence an agent is running — and it destroys the sessions it failed to respawn
into, leaving a second run worse off than the first.

The upgrade was tested rather than assumed. v1.2.1, the latest release, fails
identically on the node, despite a changelog containing
`fix(rig): detect dead tmux panes in status and start`. It declares the same bd
1.0.4 and dolt 2.0.7, so the pinned matrix would have held; it simply does not
fix this. Stale state, the socket, systemd cgroup teardown and missing binaries
were each ruled out separately.

The cost of this is not theoretical. The town has not run since 2026-08-16, which
is why the AI gateways went silent on the 17th. No bead can reach an agent, so
WP-E2 and WP-E3 cannot be demonstrated at all.

## Decision

Run agents directly, and keep Gas Town only where it is earning its place.

Dispatching a polecat is a working directory, a settings file, and
`claude -p <instructions>`. That is what wg-7yo demonstrated end to end today,
including the part that was actually in doubt: the request arrived in the AI
Gateway log tagged with its bead, the importer carried it into `usage_records`,
and the budget counted it. What Gas Town added on top of that, on this node, was
a tmux session it could not create.

The seam already exists and was built for this. `internal/gastown` is an
anti-corruption layer whose own doc comment says that keeping the boundary "makes
a later move to Gas City or another orchestrator possible without rewriting the
product model". `Orchestrator` is an interface with a named `CLIAdapter`
implementation that is still `ErrNotImplemented`. This decision is to write a
second implementation beside it rather than to finish that one.

There is precedent on both sides of the same boundary. ADR-0019 already replaced
Gas Town's Witness with a deterministic one, for a related reason: a component
whose behaviour we could not predict or test, doing a job that turned out not to
need a model at all.

## What this does NOT decide

Gas Town is not being removed. The pieces that are pulling their weight — the
rig and town layout, the beads integration, the convoy model — stay, and
`CLIAdapter` stays declared. What changes is that the agent lifecycle no longer
runs through it.

Nor is this a judgement that Gas Town is bad software. It is built to be driven
by a person at a terminal, where tmux sessions exist because somebody made them.
Our nodes have nobody at a terminal, by design, and that is the mismatch.

## Consequences

Agent dispatch stops depending on a tool that cannot start one here, and WP-E2
and WP-E3 become demonstrable again.

We take on what Gas Town was doing for us: the working directory per bead, the
lifecycle of the process, the teardown, and the reaping of a run that overstays.
Some of that already exists — the per-cell slice and its resource limits, the
runtime ceiling timer, the deterministic witness, and the budget gate that
refuses a dispatch before it starts.

`scripts/dispatch_bead.sh` becomes the reference implementation of the new path
rather than a wrapper around `gt sling`, and the parts of it fixed today — the
daemons that outlive their starting command, the readiness check that can see
success — stop being needed at all.

The upstream defect is worth reporting whether or not we depend on it: the
reproduction is one machine with no tmux sessions and one `gt up`. Filed as a
follow-up rather than as a blocker, because nothing now waits on the answer.

## Alternatives considered

**Upgrade Gas Town.** Tested. v1.2.1 fails identically, which is why this ADR
exists rather than a version bump in `versions.lock`.

**Have the platform pre-create the sessions gt expects.** Partly works, and
fights the tool: it means depending on gt's session naming, polecat sessions are
created per sling so the set is not fixed, and gt destroys the sessions it fails
on. A workaround that has to be re-derived every time gt changes.

**Wait for an upstream fix.** The reproduction is small and the maintainers are
active, so this may well be answered. It is not a plan: nothing can be
demonstrated in the meantime, and the pilot is not waiting on gt's release
schedule.
