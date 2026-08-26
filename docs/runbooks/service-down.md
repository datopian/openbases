# Runbook: a declared service is not running

**Alert rule:** `platform_service_down` · **Raised by:** `wg-monitor` on the control node

A unit the platform depends on is not active. The inbox item names each one and
gives systemd's own word for it — `inactive`, `failed`, or `unknown` when the
unit is not loaded at all.

## Why this alert exists rather than being inferred

Because the symptom hides the fault. Event freshness depends on
`workgraph-worker`, which projects deliveries on a five-second poll. When the
worker dies, `workgraph-reconcile.timer` still drains the backlog every fifteen
minutes — so deliveries keep being processed, nothing appears broken, and
freshness silently returns to its old p50 of 459 seconds against a 60-second
target (wg-95y).

The webhook check cannot catch that: it fires on a backlog older than 45 minutes
and the backlog never gets that old. Tightening it does not work either, because
with the worker down the age oscillates between zero and the reconciliation
period — any threshold below fifteen minutes flaps, and any threshold above it
never fires.

Degraded, not broken, and invisible because the fallback is good enough to hide
it. The only honest check is to ask whether the unit is running.

## 1. Which unit, and what systemd says

```bash
systemctl status <unit> --no-pager
journalctl -u <unit> -n 50 --no-pager
```

`failed` and `inactive` mean different things. `failed` means it ran and gave
up — the journal says why, and that reason is usually the real alert. `inactive`
means nothing tried to start it, which points at a deployment that did not
finish or an operator who stopped it and did not start it again.

## 2. Start it, then find out why it stopped

```bash
systemctl start <unit>
systemctl is-active <unit>
```

Starting it clears the alert on the next monitor pass, five minutes later. That
is the smaller half of the job. A unit that stopped once will stop again, and the
journal from step 1 is where the reason is — an OOM kill, a failed dependency, a
credential that was rotated without redeploying.

## 3. What was lost while it was down

Each unit has its own answer, and this is the part that does not clear itself:

| Unit | What its absence costs |
|---|---|
| `control-api` | The API was unavailable; the `platform_api_unavailable` alert should also have fired. |
| `workgraph-worker` | Deliveries were projected by reconciliation instead, every 15 minutes rather than every 5 seconds. Nothing is lost, but freshness was outside target for the whole period. |
| `workgraph-reconcile.timer` | Deliveries the worker missed were never recovered. Check for unprocessed rows older than the outage. |
| `wg-monitor.timer` | **Nothing was being watched at all**, including this. Treat the whole window as unobserved. |
| `wg-costimport.timer` | Spend was not imported; `platform_cost_import_stale` covers it and has its own runbook. |
| `wg-backup-*.timer` | Backups did not run; `platform_backup_stale` covers it. |

## 4. If the unit does not exist

`unknown` means systemd has no such unit, which is a deployment problem rather
than a runtime one. Re-run the playbook for that node:

```bash
scripts/with_secrets.sh staging bash -c 'cd infra/ansible && \
  ansible-playbook -i inventory/staging.yml site.yml'
```

If the unit is genuinely no longer wanted, remove it from `monitor_services` in
the monitor role's defaults in the same change that removes the unit. A declared
service that no longer exists is a permanently red alert, and a permanently red
alert is how the whole channel comes to be ignored.
