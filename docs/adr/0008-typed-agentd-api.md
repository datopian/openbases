# ADR-0008: Typed `agentd` API, no generic shell

- **Status:** accepted
- **Date:** 2026-08-10
- **Bead:** wg-8yv.2
- **Plan reference:** §12.4, §17.2

## Context

The control plane must make things happen on the execution node: create cells, clone repositories,
dispatch beads, stream logs, prepare pull requests, run backups. The convenient design is a
`run_shell` endpoint. That endpoint is also a complete remote-code-execution primitive: anything
that reaches the control plane, or any prompt injection that reaches an agent with API access,
inherits root-equivalent capability on the execution node.

## Decision

`agentd` accepts only typed jobs over mTLS with a signed job capability. The action set is closed
and enumerated in code. There is no generic shell, exec, or eval action. Emergency shell access is
a separate, audited administrative path with MFA, expiry, and command logging.

## Consequences

- Every new capability is a deliberate, reviewable addition to a closed set.
- A unit test scans the accepted action set for `shell`, `exec`, `run_command`, and `eval` and
  fails the build if one appears — the invariant is enforced mechanically, not by memory.
- Some operations become more work than a shell one-liner. That cost is the control.
- Jobs are refused without a signed capability even before the executor exists, so an unfinished
  daemon cannot be a foothold.

## Alternatives considered

**`run_shell` with an allow-list of command prefixes.** Rejected: prefix allow-lists are routinely
defeated by argument injection, shell metacharacters, and chained commands.
