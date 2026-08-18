# Runbooks

Plan §22 requires all thirty runbooks before go-live. Each is written by the work package that
makes it executable — a runbook for a system that does not exist yet is fiction, and fiction in a
runbook is worse than a missing file because it is trusted during an incident.

| # | Runbook | Owning work package |
|---|---|---|
| 1 | Provision a new environment | WP-B1 |
| 2 | Onboard a user | WP-C2 |
| 3 | Remove or suspend a user | WP-C2 |
| 4 | Onboard a project and repositories | WP-C3 |
| 5 | Create a restricted client execution cell | WP-E1 |
| 6 | Rotate GitHub App credentials | WP-B3 |
| 7 | Rotate an agent provider credential | WP-B3 |
| 8 | Recover a stalled or zombie agent | WP-E2 |
| 9 | Pause all agent execution | WP-E1 |
| 10 | Respond to an accidental secret exposure | WP-B3 |
| 11 | Restore PostgreSQL | WP-I2 |
| 12 | Restore a Beads database | WP-I2 |
| 13 | Restore a source snapshot and evidence chain | WP-I2 |
| 14 | Rebuild the control node | WP-I2 |
| 15 | Rebuild an execution node | WP-I2 |
| 16 | Roll back a deployment | WP-B1 |
| 17 | Upgrade or downgrade Gas Town / Beads / Dolt | WP-E2 |
| 18 | Investigate a cross-project authorisation alert | WP-I3 |
| 19 | Publish and retract marketing content | WP-G2 |
| 20 | Handle a Cloudflare Tunnel or Access outage | WP-B1 |
| 21 | Produce an audit report for a client or internal review | WP-I1 |
| 22 | Add or remove an allow-listed Google source | WP-H1 |
| 23 | Renew or recreate Workspace/Drive event subscriptions | WP-H1 |
| 24 | Reconcile missed Meet or Drive events | WP-H1 |
| 25 | Reprocess a meeting or document revision idempotently | WP-H3 |
| 26 | Respond to a source permission change or deletion | WP-H2 |
| 27 | Correct, supersede, dispute or expire a knowledge record | WP-H5 |
| 28 | Respond to malicious prompt injection in a source document | WP-H3 |
| 29 | Roll back a prompt, formula, skill or policy improvement | WP-H6 |
| 30 | Rebuild context indexes and prove permission isolation | WP-H5 |

## Format

Each runbook states: when to use it, who may run it, prerequisites and required approvals, the
exact commands, how to verify success, how to roll back, and what to record afterwards. Every
runbook must have been executed at least once — in staging where the action is destructive — before
go-live.

## Written

- [Rotate or revoke a credential](rotate-a-credential.md) — WP-B3
