# Break-glass log

Plan §16.5 requires that an emergency change carries a named actor, a record of what was done, and
a follow-up. This file is that record. Every entry must be closed out — an open entry means a
control is still relaxed.

An entry is required whenever `admin_ssh_cidrs` is non-empty, a change is made outside OpenTofu, or
any control is temporarily relaxed to diagnose or recover.

---

## 2026-08-13 — SSH opened to diagnose a failing cloud-init bootstrap

| | |
|---|---|
| Actor | Claude (coding agent), at Anu's direction |
| Environment | staging |
| Control relaxed | `admin_ssh_cidrs = ["2.72.250.110/32"]` — inbound TCP 22 from one address |
| Opened | 2026-08-13, during the WP-B1 session |
| Closed | Same session |
| Status | **Closed.** Port 22 verified closed afterwards. |

**Why.** The staging node bootstraps itself with cloud-init and dials out through Cloudflare Tunnel;
nothing dials in. The connector never came up, so the tunnel returned error 1033 and there was no
inbound path to read the bootstrap log from. Two attempts to fix it by reasoning about the template
both failed, which is what made continuing to guess the wrong approach.

**What was done.**

1. Registered a break-glass SSH key with Hetzner (`workgraph-breakglass`, id `117051669`).
2. Set `admin_ssh_key_ids` and `admin_ssh_cidrs` in `envs/staging/terraform.tfvars` — through
   OpenTofu, so the relaxed control was visible in the plan diff rather than done by hand.
3. Read `/var/log/cloud-init-output.log` and found the cause.
4. Fixed the template, redeployed, confirmed the connector came up.
5. Set `admin_ssh_cidrs = []` and re-applied. Verified port 22 closed.

**Root cause.** `templatefile` interpolates only the dollar-brace sequence. Command substitution and
arithmetic expansion are not template syntax, but they had been written as if they were, so the
doubled dollar rendered literally. A bare doubled dollar is the shell's PID, so the script failed
with a syntax error on line 17 and exited before installing anything — on a host with no way to
report it.

**Follow-ups.**

- `scripts/check_infra.py` now fails on that escaping mistake in any cloud-init template, so this
  class of bug is caught at check time rather than on an unreachable host. Verified by planting one.
- The SSH key stays registered while `admin_ssh_cidrs` stays empty. The key is inert with no rule
  admitting it, and keeping it means the next break-glass is a firewall change alone — one apply,
  no node rebuild.
- **Open gap:** a node whose bootstrap fails is still undiagnosable without opening SSH. WP-B2
  should ship the bootstrap log off-host, or the node should report bootstrap status somewhere
  reachable. Tracked as `wg-8yv.48`.
