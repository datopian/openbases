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

**And until 2026-08-26 that was still true of the graph that mattered.** The node
held a graph of two issues while the actual go-live backlog — 95 beads, every
work package, every blocker, every finding — lived in a laptop working copy whose
`bd backup` destination was another directory on the same laptop. Losing that
machine lost the backlog. wg-22k migrated it; `/srv/graphs/company-hq` is now
canonical, `scripts/hq.sh` is how anything reads or writes it, and it inherits
everything on this page: a snapshot every fifteen minutes, an immutable archive
in R2 on every offsite run, and the restore drill in wg-uh9.

**Off-machine it is one immutable `.tar.gz` per run, not a file-by-file copy of
the snapshot directory.** That directory is Dolt storage: the `.darc` chunks are
content-addressed and never change, but `manifest` is rewritten every time. The
file-by-file copy worked until the bucket lock was applied and then failed on
every run with `ObjectLockedByBucketPolicy`, because a locked object cannot be
overwritten and the manifest has to be.

The failure was worse than a missing backup and harder to see. The chunks kept
uploading, so the archive appeared to keep growing — while the off-machine
manifest was frozen at the moment the lock was applied, pinning the restorable
state to that day. It was found by a deployment failing, not by a check, and it
had been that way for two days. Each run now writes `beads-hq/<UTC>.tar.gz`,
whose name is new every time and so can never collide with a locked object, and
the run refuses to upload an archive that does not contain a manifest.

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

## What this costs

Measured on staging, not estimated.

### Local disk

| Item | Size | Notes |
| --- | --- | --- |
| One base backup | 129 MB | 111 MB cluster + 17 MB streamed WAL + 1.9 MB dump |
| 30 daily base backups | 3.9 GB | the plan's retention |
| WAL archive, 30 days | ~145 MB | 16.5 KB per compressed segment, 288/day |
| Work graph snapshot | 112 KB | plus small dated JSONL exports |
| **Steady state** | **~4 GB** | on a 160 GB disk: under 3% |

The WAL archive is the number that mattered. A segment is a **fixed 16 MB file
however little is in it**, and `archive_timeout` forces one out every 5 minutes to
keep the recovery point inside its budget. Uncompressed that measured **4.6 GB/day
— about 138 GB/month**, which would have filled the disk in roughly a month, with
no retention to stop it. The same segment gzips to ~22 KB, a factor of about 770,
because an idle segment is almost entirely zeroes. Compression plus retention
against the oldest kept base backup turns the archive from the largest thing on
the node into a rounding error.

### R2, once wg-ohk is unblocked

At $0.015/GB-month storage and $4.50/million Class A operations, with egress free:

| | If uploaded as directories | As implemented: one tar each |
| --- | --- | --- |
| Storage | 3.9 GB → $0.06/mo | **570 MB/mo → $0.01/mo** |
| Class A ops | ~73,000/mo | **~330/mo** |

Both fit inside R2's free tier (10 GB-month storage, 1M Class A operations), so
the realistic bill is **zero**, and the worst case is cents.

One tar per backup rather than the directory: a plain-format base backup is
**2,153 files** and the run tars to **18.9 MB against 129 MB** — 6.8× less storage
and one PUT instead of 2,153. On R2 the operation count is what would eventually
cost money, not the bytes.

Because nothing is ever deleted remotely, storage grows about 570 MB a month —
roughly 7 GB after a year, which crosses the free tier into a few cents a month.
A lifecycle rule is the intended answer, not client-side deletes.

Locally the backups stay **uncompressed on purpose**: `pg_verifybackup` checks a
plain backup in place, and a restore is a straight copy, which is what keeps the
recovery time at seconds. Disk on this node is abundant and restore speed is the
objective with a number attached, so the trade goes to speed locally and to size
off-machine.

### Hetzner host images

Already being paid, and the largest backup cost by a wide margin:

| Node | Type | Server | Backup surcharge (+20%) |
| --- | --- | --- | --- |
| workgraph-staging-control | cx43 | €15.99/mo | **€3.20/mo** |
| workgraph-staging-execution | cx23 | €5.49/mo | **€1.10/mo** |

**€4.30/month**, against roughly zero for everything above. If backup spend ever
needs cutting, this is the only line worth looking at — and it is also the only
copy that currently survives losing the node, so cutting it without fixing
wg-ohk first would leave nothing off-machine at all.

## Off-machine copies

`wg-backup-offsite.timer`, every 15 minutes, to `r2://workgraph-backups-staging`:

| Prefix | What | Measured |
| --- | --- | --- |
| `postgres/` | one gzipped tar per base backup | 8 objects, 18.9 MB each |
| `wal/` | the compressed WAL archive | 63 segments, 2.3 MB |
| `beads-hq/` | one gzipped tar of the work graph per run | 72 KB each |

Fifteen minutes because **the WAL sync is what sets the off-machine recovery
point**. If the node is lost, the recovery point is the newest segment that
reached R2 — not the newest one in the local archive, which died with the node.

### Retention, once the locks are applied

| Prefix | Retention | Lock |
| --- | --- | --- |
| `postgres/` | 35 days | 30 days |
| `wal/` | 35 days | 30 days |
| `beads-hq/` | 35 days | 30 days |
| `postgres-monthly/` | 400 days | 30 days |
| the evidence and audit buckets | — | **365 days** |

Every retention is longer than its lock, and that is a constraint rather than a
preference: a lifecycle rule cannot delete a locked object, so a retention
shorter than the lock would silently never take effect while looking like a
managed policy. A variable validation enforces it.

The monthly copies exist because the plan asks for thirty daily and twelve
monthly, and a single age-based rule cannot express both. `backup_offsite.sh`
writes one object per calendar month to its own prefix, which is one extra 19 MB
upload a month.

### This never deletes from R2

The central decision, and not an omission. The node holds a token that can write
to the bucket, so anything the node can delete is not a backup: an attacker who
reaches the disk also reaches the copies of it. Retention off-machine therefore
has to be a **server-side lifecycle rule**, which the node cannot influence.

The cost of never deleting is known and small: 18.9 MB a day is about 570 MB a
month and under 7 GB a year, against a 10 GB free tier.

### Two things R2 does not implement

Both cost a deployment cycle to find, so they are written down:

- **`rclone rcat` fails with 501.** Streaming an upload of unknown length uses a
  multipart flow R2 does not implement. The tar goes to a temporary file first so
  the size is known and the object goes up as a single PUT — which also allows an
  exact size comparison on read-back, a stronger check than "not empty".
- **`--no-update-modtime` is required.** R2 does not implement the `CopyObject`
  call rclone uses to rewrite a modification time, so without it every re-run
  logs a 501 per object that already exists.

And a third that was mine rather than R2's: `rclone lsf` on a path that does not
exist **exits 0 with no output**, so checking the exit status reported every
backup as already uploaded and sent nothing. The only reason that did not become
a silent total failure is that the receipt is written from a separate count of
what is actually in the bucket, which stayed at zero and refused to claim a copy
existed.

### Restoring from the off-machine copy

`test/acceptance/restore_drill_offsite.sh` — 10/10, evidence at
`docs/go-live/evidence/2026-08-22-restore-drill-offsite.json`. It reads **nothing**
from `/var/lib/workgraph`: the base backup and the WAL both come out of R2.

| Measure | Observed |
| --- | --- |
| Downloaded | 18 MB tar + 79 WAL segments |
| Fetch | 7s |
| Restore | 3s |
| **Recovery time including the fetch** | **10s** |
| Recovery point | 24s |

The fetch counts toward the recovery time, because recovering from off-machine
means downloading first and a number that excludes that is not the time anybody
experiences.

It asserts that recovery **replayed WAL fetched from R2**, not merely that the
restore succeeded. That check exists because of the bug it found: `--archive-dir`
had been accepted by the restore script from the start and then ignored, since
`restore_command` used the archive path baked into the deployed helper. Every
restore read the local archive whatever it was told. Nothing noticed, because the
only caller was a drill that wanted the local archive anyway — and it would have
quietly defeated exactly this test, passing while proving nothing.

## What is still NOT protected

The gap is no longer "everything is on one machine". What remains:

- **The R2 locks and lifecycle rules are written but NOT applied.** They are in
  `infra/tofu/modules/environment/main.tf` and plan cleanly; applying them needs
  a decision, because a bucket lock **cannot be destroyed by OpenTofu** — once
  created it stays in the API until removed by hand. Until they are applied, "the
  node cannot delete its own backups" remains true only of the script.
