# infra/ansible

WP-B2: host provisioning and hardening.

## Reaching the hosts

There is no inbound port. Ansible connects through Cloudflare Tunnel:

```
ansible → cloudflared access ssh → Cloudflare Access → tunnel → localhost:22
```

The connector already runs on the host and dialled *out*, so `localhost:22` is reachable from the
inside while the firewall stays deny-all. Access authenticates before the SSH connection is made.

Authenticate once per session:

```bash
cloudflared access login ssh-staging.openbases.com
```

That opens a browser and caches a short-lived token. For unattended runs — CI — an Access **service
token** is the right credential, but creating one needs an `Access: Service Tokens` permission the
deploy token does not currently hold (`wg-8yv.49`).

## Running

```bash
cd infra/ansible
ansible-playbook -i inventory/staging.yml site.yml
```

Idempotence is an acceptance criterion, so run it twice:

```bash
../../scripts/ansible_check.sh staging
```

That applies, then re-applies with `--check` and fails if the second run reports any change.

### Shipping only code

A full run is 168 tasks and about 3m30s on staging, most of it work that has
nothing to do with the change in front of you. For a code change:

```bash
make release && make build-linux
ansible-playbook -i inventory/staging.yml site.yml --tags code
```

Measured: 39s against 3m24s, with every binary uploaded and the service
restarted. The tag covers the binary installs **and the migration run**, which
is deliberate — see the comment above `Apply database migrations`. Do not
reach for `--start-at-task` instead: it silently skipped the API binary and
applied migrations against code two commits old, reporting success.

`--tags binaries` is the same set without the migrations, for the rare case
where you want exactly that and have thought about it.

`scripts/check_infra.py` fails if a task installs a binary without the tag, so
a new binary cannot quietly fall out of the code deploy.

Config changes — units, environment files, credentials, timers — are not in the
tag. Those get a full run.

## Roles

| Role | What it does |
|---|---|
| `base` | Hostname, base packages, time sync, unattended **security** upgrades with reboots left to an operator, bounded journald retention. |
| `ssh` | No password auth, root by key only, no agent or TCP forwarding, short login grace. Validated with `sshd -t` before it is written, so a bad config cannot lock the host out. |
| `auditd` | Watches identity and privilege files, the tunnel credential, sshd config, and the cells root; records privilege escalation. |
| `execution_cell` | One Linux user per trust domain, private home, cgroup CPU/memory/process limits, and an nftables rule blocking cell users from cloud metadata. |
| `compliance` | Reads live state and asserts on it. Independent of Ansible's own change reporting, because a task can report unchanged for the wrong reason. |

## Why the cells root is `0711`

`0711` is traversable but not listable. A cell can reach its own home; it cannot enumerate the
others. That matters because the *list of cells* leaks client names even when the contents are
unreadable.

## What is deliberately not here yet

Rootless container runtime, the observability stack, backup jobs, and the Docker runtime for control
services. Each belongs to a later work package that has something to install; adding empty roles now
would mean asserting compliance over things that do not exist.
