# The shared GitHub Actions runner for Datopian repositories.
#
# WHY THIS EXISTS. GitHub's hosted runners are billed per minute, so cost rises
# with use and a busy month hits a spending limit and blocks every repository —
# which is exactly what happened: $35 of minutes over 143 job-hours, then CI
# stopped. A machine we own costs the same whether it runs one job or ten
# thousand, which turns a variable cost with a cliff into a fixed one without.
#
# WHY NOT A VM PER JOB, which is the pattern used in datopian/postal-codes.
# Hetzner bills hourly, so a five-minute job and a one-hour job cost the same;
# for a monthly batch that is ideal, and for CI on every pull request it is
# backwards. Fifty jobs a day would cost more than a permanently running box AND
# pay sixty to ninety seconds of boot before every job.
#
# WHAT THIS IS NOT. There is no autoscaling and no second box yet. One runner
# host is a single point of failure for every repository's CI, which is an
# accepted risk at this size and the first thing to revisit when it bites — a
# second host is another EUR 8.49 and the module takes a count.

terraform {
  required_providers {
    hcloud = {
      source = "hetznercloud/hcloud"
    }
    cloudflare = {
      source = "cloudflare/cloudflare"
    }
    random = {
      source = "hashicorp/random"
    }
  }
}

locals {
  name = "gha-runner"
  common_labels = {
    managed_by = "opentofu"
    purpose    = "github-actions-runner"
  }
}

# No inbound rules, so everything is dropped.
#
# A runner needs nothing inbound: it long-polls GitHub outbound and receives
# work over that connection. Management arrives through the tunnel below.
resource "hcloud_firewall" "this" {
  name = "${local.name}-firewall"

  # Deliberately empty. A Hetzner firewall is an allow-list, so the absence of
  # rules is the policy rather than an omission — stated here because an empty
  # resource reads like unfinished work.

  labels = local.common_labels
}

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

data "cloudflare_zero_trust_tunnel_cloudflared_token" "this" {
  account_id = var.cloudflare_account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.this.id
}

resource "cloudflare_zero_trust_tunnel_cloudflared_config" "this" {
  account_id = var.cloudflare_account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.this.id

  config = {
    ingress = [
      {
        hostname = var.ssh_hostname
        service  = "ssh://localhost:22"
      },
      # Everything else is refused. The runner serves nothing.
      {
        service = "http_status:404"
      },
    ]
  }
}

resource "cloudflare_dns_record" "ssh" {
  zone_id = var.cloudflare_zone_id
  name    = var.ssh_hostname
  type    = "CNAME"
  content = "${cloudflare_zero_trust_tunnel_cloudflared.this.id}.cfargotunnel.com"
  proxied = true
  ttl     = 1
}

resource "hcloud_server" "runner" {
  name        = local.name
  server_type = var.runner_server_type
  image       = var.image
  location    = var.location
  ssh_keys    = var.admin_ssh_key_ids
  backups     = var.enable_hetzner_backups

  firewall_ids = [hcloud_firewall.this.id]

  public_net {
    ipv4_enabled = true
    ipv6_enabled = true
  }

  labels = merge(local.common_labels, { role = "runner" })

  # Same first-boot bootstrap as the Workgraph nodes: dial out to Cloudflare so
  # that Ansible has a way in, and nothing else. The runner itself is installed
  # by Ansible, not here, so a change to how runners work does not replace the
  # server.
  user_data = templatefile("${path.module}/templates/cloud-init.yaml.tftpl", {
    tunnel_token        = data.cloudflare_zero_trust_tunnel_cloudflared_token.this.token
    cloudflared_version = var.cloudflared_version
    cloudflared_sha256  = var.cloudflared_sha256
  })
}
