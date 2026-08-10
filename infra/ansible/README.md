# infra/ansible/

Idempotent host provisioning and hardening (WP-B2). A second run must produce no unexpected
changes, and a compliance script verifies the required settings independently of Ansible's own
report.

Planned roles, from plan §11.3:

| Role | Responsibility |
|---|---|
| `base` | Non-root administrative accounts, time synchronisation, unattended security updates with a controlled reboot policy, log rotation, disk quotas. |
| `ssh` | Password authentication and direct root login disabled; SSH reachable only through the approved Cloudflare path. |
| `firewall` | nftables default-deny inbound. |
| `audit` | auditd or equivalent host auditing. |
| `cloudflared` | Cloudflare Tunnel connector, outbound only, with replicas where redundancy is needed. |
| `docker` | Container runtime for control services. Agents never reach the host Docker socket. |
| `execution-cell` | A dedicated system user per cell, cgroup CPU/memory/process limits, rootless container runtime, systemd hardening, and blocked access to cloud metadata and other cells. |
| `observability` | OpenTelemetry Collector, VictoriaMetrics/VictoriaLogs, Grafana. |
| `backup` | PostgreSQL backup and WAL archiving, Beads native backup synchronisation, R2 upload. |
| `compliance` | An automated check that asserts every required setting, run in CI against staging. |

The agent user must not be able to read control-plane data or another cell. WP-B2 acceptance
requires proving this, not asserting it.
