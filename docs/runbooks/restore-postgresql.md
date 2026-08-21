# Runbook: restore PostgreSQL

Two procedures. Read the first line of each and pick.

**Verify or extract from a backup** — no outage, safe to do any time, and the
right first step in almost every incident. Section 1.

**Replace the live cluster** — an outage, irreversible, needs approval. Section 2.
Do not start here.

Everything is over the tunnel; the node has no inbound port.

```bash
ssh -o ProxyCommand="cloudflared access ssh --hostname %h" root@ssh-staging.openbases.com
```

## 0. Establish what you have before you need it

```bash
ls -1 /var/lib/workgraph/backups/postgres/          # base backups, newest last
cat /var/lib/workgraph/backups/postgres.receipt     # when the last one was verified
find /var/lib/workgraph/wal-archive -type f | wc -l # archived segments
sudo -u postgres psql -c "SELECT * FROM pg_stat_archiver"
```

`failed_count` climbing with a recent `last_failed_time` means WAL is **not**
reaching the archive, so point-in-time recovery is limited to the newest base
backup. Fix that first — it is a live data-loss window, not a backup problem.

## 1. Restore into a scratch instance (no outage)

This is what the drill does, and it answers "is the backup good" and "what did the
data look like at 14:05" without touching anything.

```bash
sudo -u postgres /usr/local/bin/wg-restore-postgres \
  --to /var/lib/postgresql/restore-check --port 5433 --verify
```

To a point in time — the moment before a bad migration, say:

```bash
sudo -u postgres /usr/local/bin/wg-restore-postgres \
  --to /var/lib/postgresql/restore-pitr --port 5433 \
  --target-time '2026-08-21 14:05:00+00'
```

Recovery **pauses** at the target rather than promoting, so you can look before
committing. Inspect it:

```bash
sudo -u postgres psql -h /var/lib/postgresql/restore-pitr -p 5433 -d workgraph
```

Pull one table back into the live database without a full restore — usually what
is actually wanted:

```bash
sudo -u postgres pg_dump -h /var/lib/postgresql/restore-pitr -p 5433 \
  -d workgraph -t some_table --data-only > /tmp/recovered.sql
```

Then stop and remove the scratch instance:

```bash
sudo -u postgres pg_ctl -D /var/lib/postgresql/restore-pitr stop
rm -rf /var/lib/postgresql/restore-pitr
```

Leaving it running is not harmless: it holds a copy of production data with its
own port, and it will be forgotten.

## 2. Replace the live cluster (outage, irreversible)

Only when the live cluster is unrecoverable. **Get approval first** and say in the
incident record who gave it.

### 2.1 Do not destroy the evidence

The current data directory is the only copy of whatever state caused this. Move
it; do not delete it.

```bash
systemctl stop control-api wg-monitor.timer
systemctl stop postgresql
mv /var/lib/postgresql/16/main /var/lib/postgresql/16/main.broken-$(date -u +%Y%m%dT%H%M%SZ)
```

If the disk cannot hold both, the disk-pressure runbook comes first — deleting the
broken cluster to make room for its replacement leaves you with nothing if the
restore fails.

### 2.2 Restore, then verify, then move it into place

Restore to a scratch path and **check it** rather than restoring straight over the
live path. A restore that fails halfway into the real location leaves neither a
working cluster nor a recoverable one.

```bash
sudo -u postgres /usr/local/bin/wg-restore-postgres \
  --to /var/lib/postgresql/restore-live --port 5433 --verify
sudo -u postgres psql -h /var/lib/postgresql/restore-live -p 5433 -d workgraph \
  -c "SELECT count(*) FROM projects" -c "SELECT count(*) FROM work_refs"
sudo -u postgres pg_ctl -D /var/lib/postgresql/restore-live stop
```

Then move it in and remove the scratch configuration. **This step matters.** The
restore script writes a self-contained minimal `postgresql.conf` and
`pg_hba.conf` into the directory, with `archive_mode = off` and a scratch port.
Debian's service starts with `-c config_file=/etc/postgresql/16/main/postgresql.conf`,
so those files are ignored once it is the live directory — but leaving them there
means the next person to read the data directory finds a configuration that is
not in effect, with archiving apparently disabled. Delete them.

```bash
mv /var/lib/postgresql/restore-live /var/lib/postgresql/16/main
cd /var/lib/postgresql/16/main
rm -f postgresql.conf pg_hba.conf pg_ident.conf postgresql.auto.conf recovery.signal restore.log
systemctl start postgresql
```

### 2.3 Confirm before declaring it over

```bash
sudo -u postgres psql -d workgraph -c "SELECT count(*) FROM pg_tables WHERE schemaname='public'"
sudo -u postgres psql -c "SHOW archive_command"   # must be wg-archive-wal, not empty
systemctl start control-api wg-monitor.timer
curl -fsS localhost:8080/health/ready && echo
sudo -u postgres psql -d workgraph -c "SELECT count(*) FROM github_deliveries WHERE processed_at IS NULL"
```

Run the delivery count twice a minute apart: it must fall. A restored database
with a stuck backlog is not a restored service.

Then take a backup immediately. The restored cluster has none of its own, and its
first scheduled one is up to a day away.

```bash
systemctl start wg-backup-postgres.service
```

### 2.4 Record it

The audit log is append-only and a restore rewrites history in the one way that
cannot be reconstructed from it. Note in the incident record: which backup, which
target time, what was lost between that point and the failure, and who approved.

## If the restore itself fails

The most common cause is a missing WAL segment: recovery needs an unbroken chain
from the base backup to the target. `restore.log` in the target directory names
the segment it wanted.

```bash
grep -iE "could not|missing|FATAL" /var/lib/postgresql/restore-live/restore.log | head
```

If the chain is broken, the newest reachable point is the end of the last
contiguous run of segments. Restore without `--target-time` to get as far as the
base backup plus its streamed WAL, which is self-contained by design.

Escalate to an organisation admin if that is not enough: the off-machine copy that
would help here does not exist yet (wg-ohk).
