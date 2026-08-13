module "environment" {
  source = "../../modules/environment"

  environment  = "staging"
  location     = var.hcloud_location
  network_cidr = "10.10.0.0/16"
  subnet_cidr  = "10.10.1.0/24"

  with_execution_node = false

  cloudflare_account_id = var.cloudflare_account_id
  cloudflare_zone_id    = var.cloudflare_zone_id
  hostname              = var.hostname
  ssh_hostname          = var.ssh_hostname

  access_allowed_emails = var.access_allowed_emails
  admin_ssh_key_ids     = var.admin_ssh_key_ids
  admin_ssh_cidrs       = var.admin_ssh_cidrs
}
