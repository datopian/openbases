# NON-SECRET environment configuration for staging.
#
# Committed deliberately: ADR-0015 requires persistent configuration to live in
# Git, or production cannot be rebuilt from code. Account and zone identifiers
# are not credentials — they appear in every dashboard URL and grant nothing on
# their own. Credentials come from the environment: HCLOUD_TOKEN,
# CLOUDFLARE_API_TOKEN and TF_ENCRYPTION.

cloudflare_account_id = "83025b28472d6aa2bf5ae59f3724aa78"

# openbases.com, deliberately not datopian.com: the deploy token needs DNS Write
# on this zone, and datopian.com carries the company Workspace MX records.
cloudflare_zone_id = "f33c8652ac5a2e9aa9526fd41c3ad2de"
hostname           = "work-staging.openbases.com"

hcloud_location = "fsn1"

# 2 vCPU / 4 GB, EUR 5.49/mo. Enough to prove cell isolation; production uses
# cx53 (16 vCPU / 32 GB) because it actually runs concurrent agent builds.
execution_server_type = "cx23"

# SSH reached through the tunnel, for Ansible. This opens no inbound port: the
# connector runs on the host and dials out, so localhost:22 is reachable from
# the inside while the firewall stays deny-all. Authentication is an Access
# service token scoped to this application alone.
ssh_hostname           = "ssh-staging.openbases.com"
ssh_hostname_execution = "ssh-exec-staging.openbases.com"

# Cloudflare Access allow-list. An empty list creates no policy at all, so the
# application denies everyone — the correct failure direction, but it also means
# nobody can log in. Adding or removing a name here is the join and leave path
# for this environment (runbooks 2 and 3).
access_allowed_emails = [
  "anuar.ustayev@datopian.com",
  "daniela.popova@datopian.com",
  "osahon.okungbowa@datopian.com",
  "rufus.pollock@datopian.com",
]

# The break-glass SSH key stays registered on the node, but no rule admits it.
# Keeping the key means a future break-glass is a firewall change alone — one
# apply, no rebuild — rather than replacing the node to add a key. The key is
# inert while admin_ssh_cidrs is empty, because nothing can reach port 22.
admin_ssh_key_ids = ["117051669"]

# CLOSED. Setting this opens a public inbound SSH port and shows loudly in the
# plan diff. It was opened on 2026-08-13 to diagnose a failing cloud-init
# bootstrap and closed the same session; see docs/runbooks/break-glass-log.md.
admin_ssh_cidrs = []

# Shared monthly agent spend ceiling, in US dollars (2026-08-15).
#
# This is the POOL, and it is now the real total. Cloudflare cannot express a
# limit spanning gateways, and applying the whole figure to each of the three
# separately made the effective ceiling $300 — three times what was agreed, for
# a control everyone believed was in force.
#
# So the pool is divided, and the shares below sum to 1. Cloudflare's own spend
# accounting then enforces the total, with no code of ours in the path: three
# limits that add up to the budget cannot together exceed it.
#
# The cost is that a domain can be refused while the pool still has room. The
# shares are weighted towards where the work actually is rather than split
# evenly, and reallocating them is a tfvars edit plus a run of
# scripts/ai_gateway_spend_limits.py. Making that automatic — reading spend and
# moving headroom between domains — is wg-o7t, and it needs a trustworthy spend
# figure first: the API exposes no spend endpoint, and the per-request cost in
# the logs is visibly wrong on small requests.
ai_monthly_budget = 100

# How the pool is divided between security domains. Must sum to 1.
#
# oss carries the pilot and every agent dispatch so far; internal and client
# have run nothing yet and are held at a floor rather than zero so that a first
# request fails on something other than the budget.
ai_budget_shares = {
  oss      = 0.7
  internal = 0.2
  client   = 0.1
}
