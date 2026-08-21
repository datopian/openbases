# Runbook: a filesystem is filling up

**Alert rule:** `platform_disk_pressure` · **Raised by:** `wg-monitor` on the control node

A filesystem has passed the used-space threshold. The inbox item **names the
filesystem** — read it before running anything, because root and the PostgreSQL
data directory fill for unrelated reasons and the wrong branch wastes the time
you do not have.

PostgreSQL stops accepting writes on a full disk, and the recovery from that is
materially worse than the prevention. Treat this as urgent even though nothing is
broken yet.

## 1. Where it went

```bash
df -h
du -x -h -d1 / 2>/dev/null | sort -h | tail -15
```

`-x` matters: without it `du` walks into other filesystems and blames the wrong
one.

## 2. The usual causes on this node, in order of likelihood

**Journald.** Logs are the most common answer and the safest to reclaim.

```bash
journalctl --disk-usage
journalctl --vacuum-size=200M
```

**The soak log.** A load-test soak writes JSON lines continuously and is not
rotated, because it is deliberately a temporary artefact of a test run.

```bash
ls -lh /var/log/wg-soak.jsonl
systemctl status wg-soak.service --no-pager   # is a soak still running on purpose?
```

If a soak has finished, summarise it before deleting the log — the summary is the
evidence, the log is not:

```bash
bash test/load/soak_report.sh /var/log/wg-soak.jsonl
```

**PostgreSQL WAL.** WAL level is raised to make backups possible, so WAL grows
faster than default. If `pg_wal` is large, check for a replication slot or a
long-running transaction holding it — deleting WAL by hand corrupts the cluster.

```bash
sudo -u postgres psql -c "SELECT slot_name, active, restart_lsn FROM pg_replication_slots"
sudo -u postgres psql -c "SELECT pid, state, xact_start, query FROM pg_stat_activity
                           WHERE xact_start < now() - interval '1 hour'"
```

**Agent working directories.** Cells accumulate clones under `/srv/cells`.

```bash
du -x -h -d2 /srv/cells | sort -h | tail
```

## 3. What not to do

Do not delete anything under `/var/lib/postgresql` directly. Do not delete
`github_deliveries` rows or audit records to reclaim space — the audit log is
append-only by design and its whole value is that it cannot be trimmed when
inconvenient. If the database genuinely needs to shrink, that is a capacity
decision and belongs to an organisation admin, not to an incident.

## 4. If it is capacity rather than waste

The node's real limits are small (2 vCPU, ~3.8 GiB RAM on staging), and the
execution cells' resource limits are **derived** from what the node has. Resizing
the node changes those derived limits on the next Ansible run, which is intended
— but the resize itself is a Hetzner change and must go through OpenTofu so it is
represented in Git.

## 5. Confirm

```bash
df -h && systemctl start wg-monitor.service && journalctl -u wg-monitor -n 20 --no-pager
```
