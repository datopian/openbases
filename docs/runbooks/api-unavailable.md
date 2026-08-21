# Runbook: the control API is unavailable

**Alert rule:** `platform_api_unavailable` · **Raised by:** `wg-monitor` on the control node

The monitor probes `GET /health/ready` on the loopback address. That endpoint
returns 200 only if the process is running **and** the database answers a ping,
so this alert means one of those two is false. `/health/live` is deliberately not
used: it returns 200 from a process that cannot reach the database, which is
exactly the state the silent password reset left the platform in for two days.

Everything below is reached over the tunnel; the node has no inbound port.

```bash
ssh -o ProxyCommand="cloudflared access ssh --hostname %h" root@ssh-staging.openbases.com
```

## 1. Which of the two is it

The alert's inbox item carries the observed status. Take the branch that matches.

**`HTTP 503`** — the process is up, the database is not reachable *from it*.

**No answer / connection refused** — the process is down or not listening.

## 2. If the answer was 503

Check whether PostgreSQL is up at all, then whether the application's credential
still works. These are different faults with the same symptom.

```bash
systemctl status postgresql --no-pager
sudo -u postgres psql -c 'SELECT 1'                      # is the server alive
journalctl -u control-api -n 50 --no-pager | grep -i 'auth\|password\|SASL'
```

`SASL authentication failed` or `password authentication failed for user
"workgraph_app"` means the credential and the database disagree. **This has
happened**: an Ansible run set the password from an environment lookup that was
empty, so the role rewrote it to nothing, reconciliation failed on every attempt,
and the backlog stopped draining silently. The repair is to put the password back
from the encrypted file, which is the source of truth:

```bash
# From a workstation with the age key. Never type the password on the node.
scripts/with_secrets.sh staging bash -c '
  ssh -o ProxyCommand="cloudflared access ssh --hostname %h" root@ssh-staging.openbases.com \
    "sudo -u postgres psql -c \"ALTER ROLE workgraph_app PASSWORD '"'"'$WG_DB_APP_PASSWORD'"'"'\""'
systemctl restart control-api
```

Then confirm the backlog drains rather than assuming it will — the backlog not
draining was the only symptom last time:

```bash
sudo -u postgres psql -d workgraph -c \
  'SELECT count(*) FROM github_deliveries WHERE processed_at IS NULL'
```

Run it twice a minute apart. The number must fall.

## 3. If there was no answer

```bash
systemctl status control-api --no-pager
journalctl -u control-api -n 100 --no-pager
```

A unit that will not start is almost always a missing credential. The service
fails closed on purpose: it refuses to start rather than run without being able
to verify an Access token, because the alternative is trusting request headers.

```bash
systemctl show control-api -p LoadCredential
ls -l /run/credentials/control-api.service/     # exists only while the unit runs
```

`243/CREDENTIALS` in the status means systemd could not load a credential file.
Check that the source file exists on disk and is readable by root.

## 4. Confirm the fix, and confirm the alert clears

```bash
curl -fsS localhost:8080/health/ready && echo
systemctl start wg-monitor.service && journalctl -u wg-monitor -n 20 --no-pager
```

Resolve the inbox item only after `/health/ready` returns 200 twice, a minute
apart. The monitor refreshes an open item rather than raising a new one, so a
still-failing check will simply reappear.

## If it is not one of these

The origin is only reachable through the tunnel. If SSH itself fails, check
whether `cloudflared` is running on the node from the Cloudflare dashboard's
tunnel view — a dead connector makes the node unreachable while it is perfectly
healthy. Escalate to an organisation admin, who can reach the Hetzner console.
