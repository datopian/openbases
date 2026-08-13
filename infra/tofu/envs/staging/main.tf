module "environment" {
  source = "../../modules/environment"

  environment  = "staging"
  location     = var.hcloud_location
  network_cidr = "10.10.0.0/16"
  subnet_cidr  = "10.10.1.0/24"

  # Staging runs an execution node so the isolation model in ADR-0002 is proven
  # on a box that can be destroyed, rather than first exercised on the node
  # holding a client's code.
  with_execution_node   = true
  execution_server_type = var.execution_server_type

  cloudflare_account_id  = var.cloudflare_account_id
  cloudflare_zone_id     = var.cloudflare_zone_id
  hostname               = var.hostname
  ssh_hostname           = var.ssh_hostname
  ssh_hostname_execution = var.ssh_hostname_execution

  access_allowed_emails = var.access_allowed_emails
  admin_ssh_key_ids     = var.admin_ssh_key_ids
  admin_ssh_cidrs       = var.admin_ssh_cidrs
}
