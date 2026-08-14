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
# Retention locks are applied to the backup and audit prefixes by WP-I2, which
# owns the retention policy.
resource "cloudflare_r2_bucket" "this" {
  for_each = toset(var.r2_buckets)

  account_id = var.cloudflare_account_id
  name       = "workgraph-${each.value}-${var.environment}"
  location   = var.r2_location
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
