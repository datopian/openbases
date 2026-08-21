# Runbook: GitHub deliveries are not being processed

**Alert rule:** `platform_webhook_backlog` · **Raised by:** `wg-monitor` on the control node

Deliveries are landing in `github_deliveries` and nothing is setting
`processed_at`. The alert fires on the **age** of the oldest unprocessed
delivery, not the depth of the queue: a burst of fifty during a busy merge is
normal and drains on the next reconciliation pass, whereas one delivery stuck for
three quarters of an hour means the consumer is broken.

The threshold is three reconciliation periods. `workgraph-reconcile.timer` fires
every 15 minutes, so a delivery that arrives just after a pass waits nearly that
long as a matter of course — a queue 20 deep whose oldest entry is 9 minutes old
is healthy, and this alert deliberately does not fire on it. **Reaching this
threshold means three consecutive passes did not consume the queue**, so do not
start by assuming a transient.

This is the failure shape that hides best. The webhook endpoint keeps accepting
deliveries and returning 200, so GitHub's delivery log is green and the problem
is invisible from outside.

## 1. Confirm and size it

```bash
sudo -u postgres psql -d workgraph -c "
  SELECT count(*) AS unprocessed,
         min(received_at) AS oldest,
         now() - min(received_at) AS age
    FROM github_deliveries WHERE processed_at IS NULL"
```

## 2. Is reconciliation running at all

```bash
systemctl list-timers 'wg-reconcile*' --no-pager
systemctl status wg-reconcile.service --no-pager
journalctl -u wg-reconcile -n 100 --no-pager
```

Two causes account for nearly all of this:

**The credential is wrong.** `SASL authentication failed` in the log. `reconcile`
shares `config.DatabaseURL()` with the API precisely so the two cannot disagree
about how to build a DSN — but they do share the password, so a password problem
takes out both. Follow step 2 of [api-unavailable.md](api-unavailable.md).

**The unit is not installed or not enabled.** A failed Ansible play can leave
handlers unfired, so the files look right and nothing was ever started. Compare
the binary's mtime against the service start time rather than trusting either:

```bash
stat -c '%y %n' /usr/local/bin/wg-reconcile
systemctl show wg-reconcile -p ActiveEnterTimestamp
```

If the binary is newer than the start, the running process is not the deployed
code. `systemctl restart wg-reconcile`.

## 3. Drain it by hand once the cause is fixed

```bash
systemctl start wg-reconcile.service
journalctl -u wg-reconcile -f          # watch one pass
```

Then verify the number actually falls, twice, a minute apart. A pass that exits 0
having processed nothing is the failure repeating, not a fix.

## 4. Deliveries that will never process

A delivery whose payload cannot be handled will block the queue behind it if
processing is ordered. Identify it before deleting anything:

```bash
sudo -u postgres psql -d workgraph -c "
  SELECT delivery_id, event_type, received_at
    FROM github_deliveries WHERE processed_at IS NULL
    ORDER BY received_at LIMIT 5"
```

Do not delete rows to clear the alert. `github_deliveries` is the record of what
GitHub told us; losing one loses the ability to reconstruct why a projection is
wrong. Recover the state from GitHub instead — reconciliation asks GitHub for
current state exactly so a lost delivery is survivable — and file a bead with the
`delivery_id` and the error.
