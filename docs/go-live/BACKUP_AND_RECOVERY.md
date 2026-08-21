# Backup and recovery (WP-I2)

What is protected, how, how fast it comes back, and — stated plainly — what is
not protected yet.

## Objectives

From plan section 15.1:

| Measure | Target | Status |
| --- | --- | --- |
| PostgreSQL recovery point | 15 minutes | met, measured by the drill |
| Beads/Dolt recovery point | 15 minutes | met by a 15-minute snapshot timer |
| Service recovery time | 4 hours | met, measured by the drill |
| Audit retention | at least 12 months | **not met** — needs off-machine storage |

## PostgreSQL

Two independent forms, because they fail differently and the day one is needed is
not the day to discover the other was the only one being taken.

**Physical, for point-in-time recovery.** A daily `pg_basebackup` in plain format,
verified against its own manifest with `pg_verifybackup` before the receipt is
written, plus continuous WAL archiving. The recovery point is set by
`archive_timeout` (5 minutes), not by the backup schedule — a daily backup alone
would put the recovery point up to 24 hours in the past.

**Logical, for the failures a physical copy reproduces faithfully.** A
`pg_dump --format=custom` taken alongside, checked with `pg_restore --list`. A
physical copy of a corrupted page is a corrupted page; a dump is also the only
form restorable into a different PostgreSQL major version.

The WAL archive lives at `/var/lib/workgraph/wal-archive`, deliberately outside
the data directory — an archive inside the tree it protects is lost by the same
`rm -rf` that made it necessary.

`archive_command` exits non-zero unless the segment is fsynced into the archive.
That choice is load-bearing: exiting zero early lets PostgreSQL recycle a segment
that was never stored, and the gap is discovered during a restore, when nothing
can be done. Failing instead means `pg_wal` grows, which the disk-pressure alert
reports (WP-I1) long before the filesystem fills.

It previously read `archive_command = '/bin/true'`, which **discarded every
segment** while `archive_mode = on` reported healthy. `archive_timeout` was also
`0`, so on a 25 MB database a segment was only archived once 16 MB of WAL
accumulated — potentially hours. Point-in-time recovery was not possible at all,
and nothing said so.

## The work graph

`bd backup sync` every 15 minutes on the control node, plus a dated JSONL export
daily.

`bd export` is **not** a backup, and Beads' own help says so: JSONL preserves
neither Dolt history nor the state `bd backup restore` consumes. The export is a
genuinely useful second recovery aid and calling it the backup is how a restore
fails.

The graph is backed up on the node because that is where WP-D2 put it. wg-ohk
recorded the previous state: the graph existed only in a working copy on one
laptop, backed up when somebody remembered.

## Restoring

`scripts/restore_postgres.sh` restores into a directory you name and starts a
**separate** instance on its own port. It never touches the live cluster, because
what you want during an incident is to confirm a backup is good before replacing
anything with it. Replacing the live data directory is a deliberate, separate act
with its own procedure: [docs/runbooks/restore-postgresql.md](../runbooks/restore-postgresql.md).

The restored instance is forced to `archive_mode = off`. Left on, it would archive
its own WAL into the same directory the live cluster uses, and after promotion its
timeline would collide with the real one.

## Host images

Hetzner daily server backups are enabled on both nodes, verified against the
Hetzner API rather than inferred from the OpenTofu default:

| Node | Daily backup window (UTC) |
| --- | --- |
| workgraph-staging-control | 14–18 |
| workgraph-staging-execution | 02–06 |

This is a genuinely off-machine copy and the only one that currently exists, so it
is what stands between the platform and losing a node. It is **not** a substitute
for the database backups above, and Hetzner says why: a live snapshot may be
inconsistent and excludes attached volumes. Restoring one gives a machine whose
PostgreSQL data directory was copied mid-write, which may or may not start.

Treat it as the floor, not the plan. Recovering the database from it is a repair
job; recovering from a verified base backup plus WAL is a procedure.

## The drill

`test/acceptance/restore_drill.sh` is timed and produces JSON evidence. It writes
a marker row into the live database, forces it into the archive, takes a real
backup, restores into a scratch instance, and requires the marker back out along
with matching row counts on five tables. "It started and has tables" would pass
against a backup of an empty database.

It is **not** destructive. It does not drop the live cluster, because a drill that
requires taking a shared environment down is a drill that gets run once and then
skipped — and the failure it exists to catch, an unrestorable backup, is caught
either way. The destructive procedure is the runbook, performed with human
approval.

## Measured, on staging

`docs/evidence/2026-08-21-restore-drill-staging.json` — 6/6 checks, produced by
the drill rather than typed here:

| Measure | Documented | Observed |
| --- | --- | --- |
| Recovery point | 900s | **7s** |
| Recovery time | 14400s | **3s** (backup 2s, restore 3s) |

Read the second row with the database size next to it: 25 MB, restored in three
seconds. It proves the procedure works, not that it scales. The number to watch as
the database grows is the restore, and the drill records the size alongside it so
a future run is comparable rather than merely reassuring.

The recovery point is 7 seconds because the drill forces a WAL switch, so it
measures the archive path rather than the wait for `archive_timeout`. Unforced,
the worst case is the 5-minute `archive_timeout` — still inside the 15-minute
objective, which is the margin that setting was chosen for.

Building this found four faults that only a real restore surfaces, every one of
which would have been discovered during an incident instead:

- the `pg_dump` was written INSIDE the `pg_basebackup` directory, so
  `pg_verifybackup` passed at backup time — the dump arrived afterwards — and
  failed for ever after, because the manifest does not list it. Backups looked
  good when taken and refused to restore;
- Debian keeps `postgresql.conf` and `pg_hba.conf` in `/etc`, not the data
  directory, so a restored copy has no configuration and will not start;
- recovery refuses to finish if `max_connections` and four related settings are
  below the source server's, which the operator cannot know at restore time — so
  the backup now records them;
- `pg_verifybackup` is not symlinked into `/usr/bin` by Debian's `pg_wrapper`,
  so the backup failed with "not installed" while the binary sat in
  `/usr/lib/postgresql/16/bin`.

## What is NOT protected

**Everything is on the same machine as the thing it protects.** That survives a
bad migration, a dropped table, a corrupted page and an accidental deletion. It
does **not** survive losing the node.

The blocker is a credential, not a design: the R2 access key reaches
`workgraph-tfstate-staging` and returns 403 on `workgraph-backups-staging`,
because it was scoped to the buckets that existed when it was minted and the
backup bucket was created afterwards. Tracked as **wg-ohk** (P0). Fixing it needs
an R2 token with Object Read & Write over the backup, evidence and audit buckets,
which is an organisation-level action.

Consequently these plan requirements are **not met**:

- copy backup data to encrypted R2 storage;
- R2 retention locks on backup and audit prefixes;
- 30 daily / 12 monthly retention (30 daily exists locally; monthly needs offsite);
- 12-month audit retention.

The monitor does not alert on the missing off-machine copy. That is deliberate and
arguable: an alert that cannot clear until somebody mints a token would sit red
indefinitely, and a permanently red alert is how the alerting system stops being
read. It is an open P0 bead and this document instead. It becomes an alert the
moment `backup_offsite_enabled` is true.
