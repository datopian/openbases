# The shared GitHub Actions runner for Datopian repositories.
#
# A separate environment from staging and production because it is a separate
# Hetzner project with a separate token — see docs/ci-runners.md for why that
# separation is the point rather than tidiness.

module "ci_runner" {
  source = "../../modules/ci-runner"

  hcloud_token          = var.hcloud_token
  cloudflare_account_id = var.cloudflare_account_id
  cloudflare_zone_id    = var.cloudflare_zone_id
  ssh_hostname          = var.ssh_hostname

  runner_server_type = var.runner_server_type
  location           = var.location
  admin_ssh_key_ids  = var.admin_ssh_key_ids

  cloudflared_version = var.cloudflared_version
  cloudflared_sha256  = var.cloudflared_sha256
}

output "ssh_hostname" {
  value = module.ci_runner.ssh_hostname
}

output "runner_ipv4" {
  value = module.ci_runner.runner_ipv4
}
