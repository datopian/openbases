# ADR-0002: Execution cells by trust boundary

- **Status:** accepted
- **Date:** 2026-08-10
- **Plan reference:** §7.4, §17.2

## Context

Agents run untrusted code: dependency installs, builds, and generated changes. If every project
shares one runtime, a compromised or merely careless agent in one project can read another client's
source, credentials, or Beads database. Isolating per employee does not help — one employee works
across several trust domains.

## Decision

The isolation unit is the **execution cell**, drawn by trust domain. A cell has a dedicated Linux
user and home, its own Gas Town workspace, its own tmux socket and Dolt port, a scoped GitHub App
installation, its own credential profile, and cgroup limits on CPU, memory, processes, and
concurrency.

Open-source and non-sensitive internal repositories may share a cell. Every restricted client
engagement gets its own. Any project holding production credentials gets its own. A cell must not
mount another cell's filesystem or credential directory. Only `agentd` creates, starts, stops, and
inspects cells.

## Consequences

- Client isolation is enforced by the operating system, not by prompt discipline.
- The registry enforces it structurally: a project marked `restricted` without its own cell is
  rejected by a database constraint, not by a code path someone can forget.
- More cells means more resource overhead and more provisioning to automate. Acceptable: the
  alternative is a single leak that ends a client relationship.
- Cells must be cheap to create, so cell provisioning is fully automated in Ansible.

## Alternatives considered

**Container-per-agent with a shared host user.** Rejected: a shared user and a reachable Docker
socket collapse the boundary the moment one container escapes or one agent reads the socket.

**One cell per employee.** Rejected: it isolates the wrong thing. The risk is cross-client data
flow, and one employee legitimately works across clients.
