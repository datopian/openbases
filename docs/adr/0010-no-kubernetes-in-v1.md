# ADR-0010: No Kubernetes in version 1

- **Status:** accepted
- **Date:** 2026-08-10
- **Plan reference:** §3.2, §6.2, §11.1

## Context

The workload is one control node, one execution node, and one staging node, serving three initial
users and three pilot projects. Kubernetes would add a control plane to operate, upgrade, secure,
and back up, plus networking and storage abstractions, in exchange for scheduling and self-healing
that this workload does not yet need.

## Decision

Deploy with Docker Compose on the control and staging nodes, and systemd units with constrained
Linux users for execution cells. Provision with OpenTofu and Ansible. No Kubernetes in version 1.

## Consequences

- Far less operational surface, and a restore path an operator can hold in their head.
- Node failure is manual recovery within the 4-hour RTO rather than automatic rescheduling. This is
  an accepted trade for a single-region internal system.
- Horizontal scaling is by adding execution nodes, not by autoscaling pods.
- If usage later justifies orchestration, immutable images and infrastructure-as-code make the move
  tractable, because nothing depends on hand-configured hosts.

## Alternatives considered

**Managed Kubernetes.** Rejected: cost and complexity out of proportion to a three-node pilot, and
it would not remove the need for the execution-cell isolation model, which is a Linux-user boundary.
