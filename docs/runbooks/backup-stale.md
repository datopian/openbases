# Runbook: a backup stream is stale or has never run

**Alert rule:** `platform_backup_stale` · **Raised by:** `wg-monitor` on the control node

A declared backup stream has not completed within its maximum age. The inbox item
names the stream and says whether it is **stale** (ran once, too long ago) or
**never** (no successful run on record).

Backup streams are declared, not discovered. That is the point: a check that
reports on the backups it can find treats the total absence of a backup as
nothing to say, which is the failure that matters most.

## How freshness is known

The monitor holds no storage credential. Each backup job writes a receipt under
`/var/lib/workgraph/backups/<stream>.receipt` **after** it has verified the remote
copy, and the monitor reads receipt timestamps. So a receipt is trustworthy about
the upload because of when it is written, not because the upload command returned
zero.

A receipt can therefore be missing for two different reasons — the job never ran,
or the job ran and the verification failed — and the job's log distinguishes them.

## 1. Which stream, and what the receipt says

```bash
ls -l /var/lib/workgraph/backups/
cat /var/lib/workgraph/backups/*.receipt
```

## 2. The `beads` stream

```bash
systemctl list-timers 'wg-backup*' --no-pager
journalctl -u wg-backup-beads -n 100 --no-pager
```

Run one by hand. It verifies the remote copy with `rclone size` before writing its
receipt, so a successful run is meaningful:

```bash
scripts/with_secrets.sh staging bash scripts/backup_beads.sh
```

**`the R2 credential cannot reach r2://…`** is the known failure, and it is a
scope problem rather than a broken key: the R2 credential in use does not cover
the backup, evidence and audit buckets (tracked as **wg-ohk**, an open decision
for an organisation admin). If that is the error, the fix is a credential with
the right scope — not a retry. Say so in the incident and escalate; do not
resolve the alert.

## 3. The `postgres` stream

The database backup and restore procedure is **WP-I2** (`wg-8yv.27`) and is not
implemented yet. If this alert names `postgres`, that is the truthful state of the
system rather than a fault to repair here: the stream is declared so that the gap
is visible in an operator's inbox instead of being discovered during a restore.

Do not silence it by removing the declaration. If the noise is genuinely
unhelpful before WP-I2 lands, snooze the inbox item — a snooze is preserved
across monitor cycles by design, and it expires, whereas a deleted declaration
does not come back.

## 4. A restore is the only proof a backup works

An untested backup is a belief. When WP-I2 lands it carries a restore drill; until
then, treat a fresh receipt as evidence the upload happened and not as evidence
the archive is usable.

```bash
# Inspect what is actually in the bucket, newest first.
scripts/with_secrets.sh staging rclone lsf r2:workgraph-backups-staging/beads/ | sort | tail -5
```

## 5. Confirm

```bash
systemctl start wg-monitor.service && journalctl -u wg-monitor -n 20 --no-pager
```
