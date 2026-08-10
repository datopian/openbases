# deploy/

| Path | Contents |
|---|---|
| `compose/` | Docker Compose definitions for the control and staging nodes. |
| `systemd/` | Unit files for `agentd` and per-cell services on the execution node. |

Production is immutable. Services pull signed release artefacts referenced by digest and versioned
configuration. Nobody edits application or infrastructure files in place during normal operation
(plan §16.4, ADR-0015).

Deployment sequence (plan §16.2):

1. reference immutable image digests;
2. verify the image signature;
3. take a pre-deploy database backup;
4. run backwards-compatible migrations;
5. deploy one service at a time;
6. pass readiness and synthetic checks;
7. keep the previous image available;
8. roll back automatically on a failed health or evaluation gate;
9. record the deployment, the configuration commit, and the evidence in the audit log and the
   company graph.
