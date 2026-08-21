# Runbook: the witness has stopped reporting

**Alert rule:** `platform_agent_health_silent` · **Raised by:** `wg-monitor` on the control node

Execution cells are deployed and no agent-health report has reached the control
plane within the silence threshold.

This alert is not about a stalled agent. A stalled agent is already reported: the
deterministic witness (ADR-0019) decides it and raises `agent_work_at_risk` or
`agent_silent` into the same inbox. **This alert is about the reporter itself
having gone quiet** — and that matters because if the witness is dead, every
future stall goes unnoticed while the inbox stays reassuringly empty. Silence
from the component whose job is to report trouble looks exactly like good news.

## 1. What the control plane last heard

```bash
sudo -u postgres psql -d workgraph -c "
  SELECT cell, max(observed_at) AS last_report, count(*) AS events
    FROM agent_health_events GROUP BY cell ORDER BY last_report DESC"
```

If one cell of several is silent, the problem is that cell. If all are, suspect
the control plane's ingest path or the Access application in front of it.

## 2. On the execution node

```bash
ssh -o ProxyCommand="cloudflared access ssh --hostname %h" root@ssh-exec-staging.openbases.com
systemctl status wg-witness.timer wg-witness.service --no-pager
journalctl -u wg-witness -n 100 --no-pager
```

The witness refuses to start if it is given a control API address and no
credentials, by design — a witness that cannot report is worse than none,
because it looks like it is working. `WG_ACCESS_CLIENT_ID` /
`WG_ACCESS_CLIENT_SECRET` are the variable names it reads; a unit still setting
`WG_SERVICE_TOKEN_*` is the old, wrong names and will never authenticate.

```bash
systemctl show wg-witness -p Environment -p LoadCredential
```

## 3. If the witness is running but nothing arrives

The path is witness → Cloudflare Access → tunnel → control API `POST
/v1/agent-health`. Each hop fails differently.

A **401 or 403** in the witness log is Access rejecting the service token. The
agent-health endpoint has its **own** audience, separate from the git-credential
endpoint on purpose — the same token grants different powers on each — so a token
minted for the wrong application authenticates and is then refused on this path.
Check that the AUD the service is configured with matches the Access application
in front of `/v1/agent-health`:

```bash
systemctl show control-api -p Environment | tr ' ' '\n' | grep ACCESS_AUD
```

A **timeout** is the tunnel. Check `cloudflared` on the execution node.

Prove the whole path end to end rather than inferring it, from a workstation:

```bash
scripts/with_secrets.sh staging bash test/acceptance/deterministic_witness.sh
```

## 4. If the cells are gone rather than the witness

A cell whose slice is absent is not running agents at all, so there is nothing to
report and the silence is honest.

```bash
systemctl status 'wgcell-*.slice' --no-pager
systemd-cgls /sys/fs/cgroup/wgcell.slice 2>/dev/null | head -20
```

If the cells were intentionally removed, the monitor's expected cell count is
stale — it comes from the deployment's inventory, so update the inventory rather
than muting the alert.

## 5. Confirm

One successful pass writes rows for every observation, not only escalations, so
recovery is visible immediately:

```bash
sudo -u postgres psql -d workgraph -c "
  SELECT cell, action, count(*) FROM agent_health_events
   WHERE observed_at > now() - interval '10 minutes' GROUP BY 1,2"
```
