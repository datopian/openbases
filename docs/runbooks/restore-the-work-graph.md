# Runbook: restore the work graph

**Alert rule:** none — this is a recovery procedure, not an alert response. The
related alert is `platform_backup_stale`
([backup-stale.md](backup-stale.md)).

The work graph is a Dolt database holding every bead, its history, and the
evidence each closure was recorded on. It lives on the control node at
`/srv/graphs/company-hq` and is snapshotted every 15 minutes.

## 0. What you have

```bash
sudo -u workgraph ls -l /var/lib/workgraph/backups/beads/
cat /var/lib/workgraph/backups/beads.receipt
cd /srv/graphs/company-hq && sudo -u workgraph bd backup status
```

There are two kinds of artefact in that directory and they are not
interchangeable:

- the **native snapshot**, which `bd backup restore` consumes and which preserves
  Dolt history;
- dated **`export-YYYYMMDD.jsonl`** files, which are a readable aid and preserve
  neither history nor Dolt state. Restoring from JSONL loses every closure record
  and every dependency edge's history. Use it only if the native snapshot is gone.

## 1. Restore into a clean database first

Never restore over the live graph as a first move. If the live graph is merely
confusing rather than broken, you will have destroyed the evidence of why.

```bash
sudo -u workgraph bash -c '
  export HOME=/srv/graphs
  cd /tmp && rm -rf graph-check && mkdir graph-check && cd graph-check
  bd backup restore /var/lib/workgraph/backups/beads
  bd list --json | head -c 400
  bd stats 2>/dev/null | head'
```

If that produces a graph with the beads you expect, the backup is good.

## 2. Replace the live graph

Approval first, and move the current graph aside rather than deleting it.

```bash
systemctl stop wg-backup-beads.timer
sudo -u workgraph mv /srv/graphs/company-hq \
  /srv/graphs/company-hq.broken-$(date -u +%Y%m%dT%H%M%SZ)
sudo -u workgraph bash -c '
  export HOME=/srv/graphs
  mkdir -p /srv/graphs/company-hq && cd /srv/graphs/company-hq
  bd backup restore /var/lib/workgraph/backups/beads'
```

Ownership is the trap here. Running `bd` as root against a `workgraph`-owned tree
leaves files owned by root, and the next scheduled snapshot fails with a
permission error that reads like a corrupt database. If anything was run as root:

```bash
chown -R workgraph:workgraph /srv/graphs/company-hq
```

## 3. Confirm, then re-enable the timer

```bash
cd /srv/graphs/company-hq
sudo -u workgraph bd list 2>&1 | tail -5
sudo -u workgraph bd ready 2>&1 | head -5
systemctl start wg-backup-beads.timer
systemctl start wg-backup-beads.service      # take one now, do not wait 15 minutes
cat /var/lib/workgraph/backups/beads.receipt
```

## 4. What you lost

Up to 15 minutes of graph changes — the snapshot interval. In practice that is a
handful of bead updates. List what changed near the boundary and re-apply by hand
if it matters; the audit log in PostgreSQL records actions taken through the API
and is a separate, independently backed-up record of the same period.

## If the native snapshot is unusable

Fall back to the newest JSONL export, understanding what it costs:

```bash
sudo -u workgraph bash -c '
  export HOME=/srv/graphs
  cd /srv/graphs/company-hq && bd import /var/lib/workgraph/backups/beads/export-YYYYMMDD.jsonl'
```

This reconstructs the beads and their current fields. It does not reconstruct
history, so "when was this closed and on what evidence" is gone for everything in
the graph. Say so explicitly in the incident record — a graph that looks complete
and has lost its history is worse than a visibly empty one, because nobody
doubts it afterwards.
