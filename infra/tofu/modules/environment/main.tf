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

  # The subnet must exist before an server joins it.
  depends_on = [hcloud_network_subnet.this]

  lifecycle {
    # Rebuilding a node is a documented runbook, not a side effect of editing
    # an image or type. Removing this is a deliberate act.
    prevent_destroy = true
  }
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

  depends_on = [hcloud_network_subnet.this]

  lifecycle {
    prevent_destroy = true
  }
}

# ---------------------------------------------------------------------------
# Cloudflare edge
# ---------------------------------------------------------------------------

# The tunnel connects outbound from the node. The origin has no inbound public
# service, so there is nothing on the internet to scan or attack directly.
resource "random_password" "tunnel_secret" {
  length  = 64
  special = false
}

resource "cloudflare_zero_trust_tunnel_cloudflared" "this" {
  account_id    = var.cloudflare_account_id
  name          = local.name
  tunnel_secret = base64encode(random_password.tunnel_secret.result)
  config_src    = "cloudflare"
}

# A proxied CNAME to the tunnel. This is the ONLY DNS record this configuration
# manages. datopian.com is a live zone with hundreds of unrelated records; any
# plan showing a deletion of a record we do not own is a bug, not an approval.
resource "cloudflare_dns_record" "app" {
  zone_id = var.cloudflare_zone_id
  name    = var.hostname
  type    = "CNAME"
  content = "${cloudflare_zero_trust_tunnel_cloudflared.this.id}.cfargotunnel.com"
  proxied = true
  ttl     = 1
  comment = "Managed by OpenTofu — workgraph ${var.environment}. Do not edit by hand."
}

# An empty allow-list creates no policy at all. An Access application with no
# policy denies everyone, which is the correct failure direction for a system
# holding client-derived data — and it avoids inventing a placeholder identity.
resource "cloudflare_zero_trust_access_policy" "allowed_users" {
  count = length(var.access_allowed_emails) > 0 ? 1 : 0

  account_id = var.cloudflare_account_id
  name       = "${local.name}-allowed-users"
  decision   = "allow"

  # One include rule per permitted address. Indexing the first element here
  # would silently authorise only one person out of the list.
  include = [
    for address in var.access_allowed_emails : {
      email = {
        email = address
      }
    }
  ]
}

resource "cloudflare_zero_trust_access_application" "this" {
  account_id                = var.cloudflare_account_id
  name                      = local.name
  domain                    = var.hostname
  type                      = "self_hosted"
  session_duration          = var.access_session_duration
  auto_redirect_to_identity = false

  # The application enforces identity at the edge; the control API independently
  # validates the Access JWT rather than trusting headers (ADR-0006).
  http_only_cookie_attribute = true

  # The policy must be attached, or the application would exist with no rules.
  policies = [
    for policy in cloudflare_zero_trust_access_policy.allowed_users : {
      id         = policy.id
      precedence = 1
    }
  ]
}

# ---------------------------------------------------------------------------
# R2
# ---------------------------------------------------------------------------

# Backups, encrypted evidence snapshots, audit exports and remote state.
# Retention locks are applied to the backup and audit prefixes by WP-I2, which
# owns the retention policy.
resource "cloudflare_r2_bucket" "this" {
  for_each = toset(var.r2_buckets)

  account_id = var.cloudflare_account_id
  name       = "workgraph-${each.value}-${var.environment}"
  location   = var.r2_location
}
