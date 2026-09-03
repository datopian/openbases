# Runbook: a work graph cannot be read by the service user

**Alert rule:** `platform_graph_unreadable` · **Raised by:** `wg-monitor` on the control node

A declared work graph is missing, or its Dolt manifest cannot be read by the
account the services run as. The inbox item names the graph, the path, and which
of the two it is.

This is a present outage rather than a warning about the future. Every bead,
every dispatch and every projection goes through the graph, so a graph the
service user cannot open has stopped the work rather than threatened it.

## Why this check exists

On 3 September 2026 three files under `/srv/graphs/company-hq/.beads` were left
root-owned. The service user could not read its own database, and the only thing
that noticed was `wg-backup-beads.service` failing:

```
Error: failed to open database: embeddeddolt: init schema: failed to load
database "wg": open /srv/graphs/company-hq/.beads/embeddeddolt/wg/.dolt/noms/manifest:
permission denied
```

The monitor checked disk, backups, webhooks, services, cost imports and
Workspace sources — and not the database the entire work graph lives in. The
failure was found by reading a backup failure, which is luck rather than
observability.

**The check runs as the service user, not as root.** That distinction is the
whole check: the files were present and intact, so anything performed as root
would have reported a healthy graph.

## 1. Confirm what the service user sees

```bash
scripts/on.sh staging control \
  'sudo -u workgraph test -r /srv/graphs/company-hq/.beads/embeddeddolt/wg/.dolt/noms/manifest \
     && echo READABLE || echo NOT-READABLE'
```

Then look at the ownership of the whole graph, not just the manifest — whatever
wrote the file as root usually wrote several:

```bash
scripts/on.sh staging control \
  'sudo find /srv/graphs/company-hq -not -user workgraph -printf "%u %p\n" | head -20'
```

## 2. Repair it the way the playbook does

Do **not** chown by hand as a first move. The `beads_hq` role has a task called
"Own the graph, whatever last wrote it" which does exactly this, and running the
playbook leaves the node in a state the next deploy agrees with:

```bash
scripts/with_secrets.sh staging ansible-playbook \
  -i infra/ansible/inventory/staging.yml infra/ansible/site.yml \
  --limit workgraph-staging-control
```

Note this needs a FULL run: the graph ownership task is in `beads_hq`, which the
`code` tag does not cover.

Confirm afterwards:

```bash
scripts/on.sh staging control 'sudo find /srv/graphs -not -user workgraph | head'
scripts/hq.sh list | head -3
```

## 3. Find what wrote as root

A repair that does not answer this will be needed again. The known cause is a
command run as root, or `sudo -u workgraph bd` **without** setting `HOME` — the
role sets `HOME=<graph dir>` and `scripts/hq.sh` did not, so `bd` wrote Dolt
config into the wrong home and, in some paths, as the wrong user. That is
tracked as **wg-5i8**.

Check the journal around the first failure:

```bash
scripts/on.sh staging control 'sudo journalctl --since "-24h" | grep -iE "dolt|beads" | head -40'
```

## If the graph is MISSING rather than unreadable

Different fix, and do not run the playbook first — a role that creates the graph
on first sight will happily create an empty one, and an empty graph looks
healthy. Restore from the most recent backup instead:

```bash
scripts/on.sh staging control 'ls -la /var/lib/workgraph/backups/'
```

The graph is backed up by `wg-backup-beads.service` and copied off the machine by
`wg-backup-offsite.service`. Restore, verify `bd list` returns the expected
count, and only then let a deploy near it.

## Related

- `platform_backup_stale` — the graph backup failing is often the first symptom
  of this, which is how the original incident was found.
- **wg-5i8** — `scripts/hq.sh` runs `bd` without `HOME`, the probable cause.
