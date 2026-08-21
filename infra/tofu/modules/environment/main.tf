# One Workgraph environment: private network, hardened nodes, Cloudflare edge,
# and the R2 buckets that hold backups, evidence and audit exports.
#
# Design constraints this module enforces (plan sections 11.1-11.3, ADR-0006):
#   - no inbound public services; the firewall is deny-by-default
#   - the origin is reached only through Cloudflare Tunnel, outbound-only
#   - the control plane database and untrusted agent workloads never share a VM
#   - everything is recreatable from code; nothing is configured by hand

locals {
  name    = "workgraph-${var.environment}"
  is_prod = var.environment == "production"

  common_labels = {
    project     = "workgraph"
    environment = var.environment
    managed_by  = "opentofu"
  }
}

# ---------------------------------------------------------------------------
# Network
# ---------------------------------------------------------------------------

resource "hcloud_network" "this" {
  name     = local.name
  ip_range = var.network_cidr
  labels   = local.common_labels
}

resource "hcloud_network_subnet" "this" {
  network_id   = hcloud_network.this.id
  type         = "cloud"
  network_zone = "eu-central"
  ip_range     = var.subnet_cidr
}

# Spread placement keeps the control and execution nodes off the same physical
# host, so one hardware failure cannot take both.
resource "hcloud_placement_group" "this" {
  count  = var.with_execution_node ? 1 : 0
  name   = local.name
  type   = "spread"
  labels = local.common_labels
}

# ---------------------------------------------------------------------------
# Firewall
# ---------------------------------------------------------------------------

# Hetzner cloud firewalls are allow-lists: with no inbound rules, every inbound
# connection is dropped. That is deliberate and is the whole posture. SSH is
# added only if admin_ssh_cidrs is non-empty, which shows up loudly in a plan.
resource "hcloud_firewall" "this" {
  name   = "${local.name}-default-deny"
  labels = local.common_labels

  dynamic "rule" {
    for_each = length(var.admin_ssh_cidrs) > 0 ? [1] : []
    content {
      description = "BREAK-GLASS SSH — must be removed after use"
      direction   = "in"
      protocol    = "tcp"
      port        = "22"
      source_ips  = var.admin_ssh_cidrs
    }
  }

  # Outbound is unrestricted: nodes need Cloudflare, GitHub, package registries
  # and agent providers. Egress filtering is a later hardening step, tracked
  # with WP-B2 rather than guessed at here.
  rule {
    direction       = "out"
    protocol        = "tcp"
    port            = "any"
    destination_ips = ["0.0.0.0/0", "::/0"]
  }

  rule {
    direction       = "out"
    protocol        = "udp"
    port            = "any"
    destination_ips = ["0.0.0.0/0", "::/0"]
  }

  rule {
    direction       = "out"
    protocol        = "icmp"
    destination_ips = ["0.0.0.0/0", "::/0"]
  }
}

# ---------------------------------------------------------------------------
# Nodes
# ---------------------------------------------------------------------------

# The control node runs the API, PostgreSQL, the job worker, observability and
# the tunnel connector. It never runs agent worktrees or project credentials.
resource "hcloud_server" "control" {
  name        = "${local.name}-control"
  server_type = var.control_server_type
  image       = var.image
  location    = var.location
  ssh_keys    = var.admin_ssh_key_ids
  backups     = var.enable_hetzner_backups

  firewall_ids       = [hcloud_firewall.this.id]
  placement_group_id = var.with_execution_node ? hcloud_placement_group.this[0].id : null

  public_net {
    ipv4_enabled = true
    ipv6_enabled = true
  }

  network {
    network_id = hcloud_network.this.id
    ip         = cidrhost(var.subnet_cidr, 10)
  }

  labels = merge(local.common_labels, { role = "control" })

  # First-boot bootstrap. The host dials out to Cloudflare; nothing dials in.
  user_data = templatefile("${path.module}/templates/cloud-init.yaml.tftpl", {
    tunnel_token        = data.cloudflare_zero_trust_tunnel_cloudflared_token.control.token
    cloudflared_version = var.cloudflared_version
    cloudflared_sha256  = var.cloudflared_sha256
  })

  # The subnet must exist before a server joins it.
  depends_on = [hcloud_network_subnet.this]

  # No prevent_destroy. It was here as belt-and-braces, but it contradicts the
  # WP-B1 acceptance criterion that staging can be destroyed and recreated from
  # code, and it blocks the replacement that a cloud-init change requires.
  #
  # Protection comes from the approval path instead: infrastructure.destroy
  # needs two approvers under policies/default.yaml, every plan is inspected for
  # deletes before approval, and make live-zones runs afterwards.
}

# The execution node runs Gas Town cells, Dolt, worktrees and build caches.
# Agent builds are noisy and can starve or compromise the system of record, so
# they never share a machine with it (plan section 1.3).
resource "hcloud_server" "execution" {
  count = var.with_execution_node ? 1 : 0

  name        = "${local.name}-execution"
  server_type = var.execution_server_type
  image       = var.image
  location    = var.location
  ssh_keys    = var.admin_ssh_key_ids
  backups     = var.enable_hetzner_backups

  firewall_ids       = [hcloud_firewall.this.id]
  placement_group_id = hcloud_placement_group.this[0].id

  public_net {
    ipv4_enabled = true
    ipv6_enabled = true
  }

  network {
    network_id = hcloud_network.this.id
    ip         = cidrhost(var.subnet_cidr, 20)
  }

  labels = merge(local.common_labels, { role = "execution" })

  user_data = templatefile("${path.module}/templates/cloud-init.yaml.tftpl", {
    tunnel_token        = data.cloudflare_zero_trust_tunnel_cloudflared_token.execution[0].token
    cloudflared_version = var.cloudflared_version
    cloudflared_sha256  = var.cloudflared_sha256
  })

  depends_on = [hcloud_network_subnet.this]
}

# ---------------------------------------------------------------------------
# Cloudflare edge
# ---------------------------------------------------------------------------

# One tunnel PER NODE, not per environment.
#
# A single tunnel shared by two nodes means two connectors serving identical
# ingress, and Cloudflare routes to whichever it likes. SSH then lands on an
# arbitrary host and the application hostname can reach a node with no
# application on it. Per-node tunnels make every hostname deterministic.

resource "random_password" "tunnel_secret_control" {
  length  = 64
  special = false
}

resource "cloudflare_zero_trust_tunnel_cloudflared" "control" {
  account_id    = var.cloudflare_account_id
  name          = "${local.name}-control"
  tunnel_secret = base64encode(random_password.tunnel_secret_control.result)
  config_src    = "cloudflare"
}

data "cloudflare_zero_trust_tunnel_cloudflared_token" "control" {
  account_id = var.cloudflare_account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.control.id
}

resource "cloudflare_zero_trust_tunnel_cloudflared_config" "control" {
  account_id = var.cloudflare_account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.control.id

  config = {
    ingress = concat(
      var.ssh_hostname != "" ? [
        {
          hostname = var.ssh_hostname
          service  = "ssh://localhost:22"
        }
      ] : [],
      [
        {
          hostname = var.hostname
          service  = "http://localhost:${var.app_port}"
        },
        {
          service = "http_status:404"
        },
      ]
    )
  }
}

resource "random_password" "tunnel_secret_execution" {
  count   = var.with_execution_node ? 1 : 0
  length  = 64
  special = false
}

resource "cloudflare_zero_trust_tunnel_cloudflared" "execution" {
  count = var.with_execution_node ? 1 : 0

  account_id    = var.cloudflare_account_id
  name          = "${local.name}-execution"
  tunnel_secret = base64encode(random_password.tunnel_secret_execution[0].result)
  config_src    = "cloudflare"
}

data "cloudflare_zero_trust_tunnel_cloudflared_token" "execution" {
  count = var.with_execution_node ? 1 : 0

  account_id = var.cloudflare_account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.execution[0].id
}

# The execution node exposes SSH only. It never serves the application: agent
# workloads and the control plane stay on separate machines (plan section 1.3).
resource "cloudflare_zero_trust_tunnel_cloudflared_config" "execution" {
  count = var.with_execution_node ? 1 : 0

  account_id = var.cloudflare_account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.execution[0].id

  config = {
    ingress = concat(
      var.ssh_hostname_execution != "" ? [
        {
          hostname = var.ssh_hostname_execution
          service  = "ssh://localhost:22"
        }
      ] : [],
      [
        {
          service = "http_status:404"
        },
      ]
    )
  }
}

# ---------------------------------------------------------------------------
# DNS and Access, one record per hostname
# ---------------------------------------------------------------------------

resource "cloudflare_dns_record" "app" {
  zone_id = var.cloudflare_zone_id
  name    = var.hostname
  type    = "CNAME"
  content = "${cloudflare_zero_trust_tunnel_cloudflared.control.id}.cfargotunnel.com"
  proxied = true
  ttl     = 1
  comment = "Managed by OpenTofu — workgraph ${var.environment}. Do not edit by hand."
}

resource "cloudflare_dns_record" "ssh" {
  count = var.ssh_hostname != "" ? 1 : 0

  zone_id = var.cloudflare_zone_id
  name    = var.ssh_hostname
  type    = "CNAME"
  content = "${cloudflare_zero_trust_tunnel_cloudflared.control.id}.cfargotunnel.com"
  proxied = true
  ttl     = 1
  comment = "Managed by OpenTofu — workgraph ${var.environment} control SSH."
}

resource "cloudflare_dns_record" "ssh_execution" {
  count = var.with_execution_node && var.ssh_hostname_execution != "" ? 1 : 0

  zone_id = var.cloudflare_zone_id
  name    = var.ssh_hostname_execution
  type    = "CNAME"
  content = "${cloudflare_zero_trust_tunnel_cloudflared.execution[0].id}.cfargotunnel.com"
  proxied = true
  ttl     = 1
  comment = "Managed by OpenTofu — workgraph ${var.environment} execution SSH."
}

resource "cloudflare_zero_trust_access_policy" "allowed_users" {
  count = length(var.access_allowed_emails) > 0 ? 1 : 0

  account_id = var.cloudflare_account_id
  name       = "${local.name}-allowed-users"
  decision   = "allow"

  include = [
    for address in var.access_allowed_emails : {
      email = {
        email = address
      }
    }
  ]
}

resource "cloudflare_zero_trust_access_application" "this" {
  account_id                 = var.cloudflare_account_id
  name                       = local.name
  domain                     = var.hostname
  type                       = "self_hosted"
  session_duration           = var.access_session_duration
  auto_redirect_to_identity  = false
  http_only_cookie_attribute = true

  policies = [
    for policy in cloudflare_zero_trust_access_policy.allowed_users : {
      id         = policy.id
      precedence = 1
    }
  ]
}

# GitHub cannot complete an Access challenge, so the webhook path is admitted
# without one. This is NOT unauthenticated: the endpoint verifies an HMAC
# signature over the payload with a constant-time comparison before parsing
# anything, and refuses a delivery with no idempotency key.
#
# A path-scoped application with a bypass policy, so the exception is exactly
# one route on one hostname rather than a hole in the application as a whole.
resource "cloudflare_zero_trust_access_policy" "webhook_bypass" {
  account_id = var.cloudflare_account_id
  name       = "${local.name}-github-webhook-bypass"
  decision   = "bypass"

  include = [
    {
      everyone = {}
    }
  ]
}

resource "cloudflare_zero_trust_access_application" "github_webhook" {
  account_id       = var.cloudflare_account_id
  name             = "${local.name}-github-webhook"
  domain           = "${var.hostname}/v1/integrations/github/webhook"
  type             = "self_hosted"
  session_duration = "0s"

  policies = [
    {
      id         = cloudflare_zero_trust_access_policy.webhook_bypass.id
      precedence = 1
    }
  ]
}

resource "cloudflare_zero_trust_access_application" "ssh" {
  count = var.ssh_hostname != "" ? 1 : 0

  account_id       = var.cloudflare_account_id
  name             = "${local.name}-ssh"
  domain           = var.ssh_hostname
  type             = "self_hosted"
  session_duration = "1h"

  # Humans first, then the service token. The email policy is evaluated at
  # precedence 1 so an operator's identity is what matches when both could,
  # keeping the audit trail attributed to a person rather than to automation.
  policies = concat(
    [
      for policy in cloudflare_zero_trust_access_policy.allowed_users : {
        id         = policy.id
        precedence = 1
      }
    ],
    [{
      id         = cloudflare_zero_trust_access_policy.ssh_service.id
      precedence = 2
    }]
  )
}

resource "cloudflare_zero_trust_access_application" "ssh_execution" {
  count = var.with_execution_node && var.ssh_hostname_execution != "" ? 1 : 0

  account_id       = var.cloudflare_account_id
  name             = "${local.name}-ssh-execution"
  domain           = var.ssh_hostname_execution
  type             = "self_hosted"
  session_duration = "1h"

  # Humans first, then the service token. The email policy is evaluated at
  # precedence 1 so an operator's identity is what matches when both could,
  # keeping the audit trail attributed to a person rather than to automation.
  policies = concat(
    [
      for policy in cloudflare_zero_trust_access_policy.allowed_users : {
        id         = policy.id
        precedence = 1
      }
    ],
    [{
      id         = cloudflare_zero_trust_access_policy.ssh_service.id
      precedence = 2
    }]
  )
}

# ---------------------------------------------------------------------------
# R2
# ---------------------------------------------------------------------------

# Backups, encrypted evidence snapshots, audit exports and remote state.
resource "cloudflare_r2_bucket" "this" {
  for_each = toset(var.r2_buckets)

  account_id = var.cloudflare_account_id
  name       = "workgraph-${each.value}-${var.environment}"
  location   = var.r2_location
}

# ---------------------------------------------------------------------------
# Retention locks (WP-I2, wg-y9w)
# ---------------------------------------------------------------------------
#
# The off-machine backup script never deletes from R2, deliberately. But that is
# a property of the SCRIPT: the control node holds a token with object write
# access, so anyone who reaches the node can delete what the node can reach, and
# a backup an attacker can delete is not a backup.
#
# A bucket lock moves the guarantee into the bucket. Within the lock period an
# object cannot be deleted or overwritten by anybody holding the token — the node,
# a stolen copy of its credential, or a mistaken operator.
#
# What it does NOT protect against is worth stating: a compromised node can stop
# new backups being written. That is why the monitor alerts on staleness rather
# than only on absence — the two failures look identical in the bucket, and only
# one of them is visible there.
#
# Locks are also close to irreversible by design, which is the point and the
# risk. Shortening one does not retroactively unlock objects already written, so
# a lock period set too long is lived with rather than corrected.
resource "cloudflare_r2_bucket_lock" "this" {
  for_each = cloudflare_r2_bucket.this

  account_id  = var.cloudflare_account_id
  bucket_name = each.value.name

  rules = [{
    id      = "workgraph-${each.key}-lock"
    enabled = true
    # No prefix: everything in the bucket. A prefix would leave anything written
    # outside it unprotected, and the whole point is that there is no gap for an
    # attacker to write into and delete from.
    condition = {
      type            = "Age"
      max_age_seconds = (each.key == "backups" ? var.r2_lock_days : var.r2_audit_lock_days) * 24 * 60 * 60
    }
  }]
}

# ---------------------------------------------------------------------------
# Lifecycle: retention that is enforced rather than achieved by never deleting
# ---------------------------------------------------------------------------
#
# Without this the off-machine copies grow for ever, because nothing deletes
# them. That is cheap (about 570 MB a month) and it is not a policy — the plan
# asks for thirty daily and twelve monthly, which is a statement about what is
# kept AND what is not.
#
# A single age-based rule cannot express both, so the daily and monthly copies
# live under separate prefixes with separate rules. The monthly copy is written
# by scripts/backup_offsite.sh, one per calendar month.
#
# Every retention here is longer than the lock period. It has to be: a lifecycle
# rule cannot delete a locked object, so a shorter retention would silently never
# take effect while looking like a managed policy.
resource "cloudflare_r2_bucket_lifecycle" "backups" {
  account_id  = var.cloudflare_account_id
  bucket_name = cloudflare_r2_bucket.this["backups"].name

  rules = [
    for r in [
      { key = "postgres-daily", prefix = "postgres/", days = var.r2_daily_retention_days },
      { key = "wal", prefix = "wal/", days = var.r2_daily_retention_days },
      { key = "beads-hq", prefix = "beads-hq/", days = var.r2_daily_retention_days },
      # The prefix the workstation script wrote to before the graph moved to the
      # control node. Expired on the monthly schedule rather than deleted here,
      # so the history is not destroyed by a refactor.
      { key = "beads-legacy", prefix = "beads/", days = var.r2_monthly_retention_days },
      { key = "postgres-monthly", prefix = "postgres-monthly/", days = var.r2_monthly_retention_days },
      ] : {
      id         = "workgraph-${r.key}"
      enabled    = true
      conditions = { prefix = r.prefix }
      delete_objects_transition = {
        condition = {
          type    = "Age"
          max_age = r.days * 24 * 60 * 60
        }
      }
      # Abandoned multipart uploads are billed as storage and are invisible in a
      # normal listing, so they accumulate silently. A week is generous for an
      # upload that should take seconds.
      abort_multipart_uploads_transition = {
        condition = {
          type    = "Age"
          max_age = 7 * 24 * 60 * 60
        }
      }
    }
  ]
}

# ---------------------------------------------------------------------------
# Unattended access for Ansible and drift detection (wg-8yv.49)
# ---------------------------------------------------------------------------

# A service token for automation that cannot complete an interactive login.
#
# The SSH applications enforce the same email allow-list as the application
# itself, so reaching a node requires a human at a browser. That is right for an
# operator and impossible for a scheduled job, and drift detection is a
# scheduled job by definition (plan section 16.5).
#
# The trade is real and deliberate: this credential opens an SSH session to a
# node without any human present. It is therefore scoped to the SSH applications
# alone, given a finite lifetime, and its rotation is a runbook step rather than
# something left open-ended.
resource "cloudflare_zero_trust_access_service_token" "automation" {
  account_id = var.cloudflare_account_id
  name       = "workgraph-${var.environment}-automation"

  # Finite by design. An expiring credential forces rotation to be a practised
  # procedure rather than an emergency one discovered during an incident.
  duration = "8760h"
}

# The non-identity policy that accepts it.
#
# Separate from the email policy rather than merged into it: a service token and
# a human are different subjects with different revocation stories, and keeping
# them apart means revoking one never disturbs the other.
resource "cloudflare_zero_trust_access_policy" "ssh_service" {
  account_id = var.cloudflare_account_id
  name       = "workgraph-${var.environment}-ssh-automation"
  decision   = "non_identity"

  include = [{
    service_token = {
      token_id = cloudflare_zero_trust_access_service_token.automation.id
    }
  }]
}

# ---------------------------------------------------------------------------
# Execution cells calling the control API (WP-E3)
# ---------------------------------------------------------------------------

# A service token for execution cells, separate from the automation one.
#
# Separate because the blast radii differ. The automation token opens SSH
# sessions to nodes; this one only asks the control plane for a git credential.
# Sharing one token would mean a compromised cell could open a shell on the
# control node, which is the opposite of what cells are for.
resource "cloudflare_zero_trust_access_service_token" "cells" {
  account_id = var.cloudflare_account_id
  name       = "workgraph-${var.environment}-cells"
  duration   = "8760h"
}

# The token is accepted on exactly ONE path.
#
# Scoped like the webhook exception rather than added to the application policy,
# because a service token allowed on the whole API could read every project's
# work with no human identity attached — and row-level security would have
# nothing to filter on.
resource "cloudflare_zero_trust_access_application" "cell_token_mint" {
  account_id       = var.cloudflare_account_id
  name             = "${local.name}-cell-token-mint"
  domain           = "${var.hostname}/v1/integrations/github/installation-token"
  type             = "self_hosted"
  session_duration = "0s"

  policies = [{
    id         = cloudflare_zero_trust_access_policy.cells_service.id
    precedence = 1
  }]
}

# The deterministic witness reports agent health from the same execution nodes
# with the same token (ADR-0019).
#
# Its own application rather than a second path on the one above: a Zero Trust
# application matches one domain, and widening the existing one to a prefix
# would quietly extend the token's reach to every path underneath it. Two narrow
# applications sharing one policy keeps the grant equal to the two endpoints
# that were actually reviewed.
resource "cloudflare_zero_trust_access_application" "cell_agent_health" {
  account_id       = var.cloudflare_account_id
  name             = "${local.name}-cell-agent-health"
  domain           = "${var.hostname}/v1/agent-health"
  type             = "self_hosted"
  session_duration = "0s"

  policies = [{
    id         = cloudflare_zero_trust_access_policy.cells_service.id
    precedence = 1
  }]
}

resource "cloudflare_zero_trust_access_policy" "cells_service" {
  account_id = var.cloudflare_account_id
  name       = "workgraph-${var.environment}-cells-service"
  decision   = "non_identity"

  include = [{
    service_token = {
      token_id = cloudflare_zero_trust_access_service_token.cells.id
    }
  }]
}
