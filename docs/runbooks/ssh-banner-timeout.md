# SSH times out during banner exchange

```
Connection timed out during banner exchange
Connection to UNKNOWN port 65535 timed out
```

Ansible reports the host `unreachable`. Everything else about the node looks
fine. **Check your own machine first** — this message points at the server and
the cause is usually local.

## What it actually means

TCP connected and nothing spoke SSH. `cloudflared` handed `ssh` a stream that
was not an SSH server, so `ssh` waited for a banner that never came. The error
describes the last thing `ssh` was doing, not the thing that was wrong.

## Check in this order

**1. A stale cloudflared token lock.** This is the likely one.

```bash
ls -la ~/.cloudflared/ | grep "$HOSTNAME"
```

A `-token.lock` file with **no `-token` file beside it**, or a token of zero
bytes, means an earlier `cloudflared access` was killed mid-authentication and
left the lock behind. Every later attempt then produces this timeout.

```bash
rm -f ~/.cloudflared/<hostname>-*-token.lock
```

Then retry. It is a lock file, not a credential: removing it costs nothing and
the next run acquires a token normally.

This happens after any interrupted run — a command that hit a timeout, a
cancelled deploy, Ctrl-C at the wrong moment.

**2. Is the node actually up?** The comparison that settles it quickly:

```bash
# The API goes through the SAME cloudflared as SSH, on a different hostname.
gh workflow run external-probe.yml && sleep 40 && gh run list --workflow external-probe.yml --limit 1
```

A passing probe means cloudflared is connected and the node is serving. If HTTP
works and SSH does not, the tunnel is not the problem.

**3. Is the other node reachable?** `scripts/on.sh staging execution 'uptime'`.
One node working and the other not is a per-hostname problem — a token, a lock —
rather than an outage.

**4. The tunnel's own view**, from Cloudflare rather than from inference:

```bash
scripts/with_secrets.sh staging python3 - <<'PY'
import json,os,urllib.request
acct=os.environ["CLOUDFLARE_ACCOUNT_ID"]; tok=os.environ["CLOUDFLARE_API_TOKEN"]
def get(p):
    r=urllib.request.Request(f"https://api.cloudflare.com/client/v4{p}",
        headers={"Authorization":f"Bearer {tok}","User-Agent":"workgraph/1"})
    return json.load(urllib.request.urlopen(r))
for t in get(f"/accounts/{acct}/cfd_tunnel?is_deleted=false")["result"]:
    if "workgraph" not in t["name"]: continue
    print(t["name"], t.get("status"), len(t.get("connections") or []), "connections")
    cfg=get(f"/accounts/{acct}/cfd_tunnel/{t['id']}/configurations")
    for i in (cfg["result"]["config"] or {}).get("ingress") or []:
        print("   ", i.get("hostname","(catch-all)"), "->", i.get("service"))
PY
```

`status=healthy` with the expected `ssh://localhost:22` ingress rules out both
the tunnel and its configuration.

**5. Host resources**, if the above is all clean. Hetzner's metrics need no SSH:

```bash
scripts/with_secrets.sh staging python3 -c '
import json,os,urllib.request,datetime
tok=os.environ["HCLOUD_TOKEN"]
def get(p):
    return json.load(urllib.request.urlopen(urllib.request.Request(
        "https://api.hetzner.cloud/v1"+p, headers={"Authorization":f"Bearer {tok}"})))
for s in get("/servers")["servers"]:
    print(s["name"], s["status"])'
```

A node that is out of memory or disk accepts a TCP connection and never forks a
session, which looks identical. CPU near zero and normal disk writes rule it out.

## What this cost, and why the order above

Diagnosed 2026-08-31. The node had 17 days of uptime and a load average of 0.06
throughout. It was investigated as an outage — tunnel config, connector health,
Hetzner metrics — before anyone looked in `~/.cloudflared`, and a report went out
saying the control node was unadministrable and that monitoring had a gap.

Both were wrong. The monitoring was correct: the external probe kept passing
because the node was genuinely healthy.

The lesson is the order. A message that names the remote host invites you to
investigate the remote host, and one hostname failing while another succeeds
through the same tunnel is a client-side problem almost every time.
