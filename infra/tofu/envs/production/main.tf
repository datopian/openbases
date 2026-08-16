module "environment" {
  source = "../../modules/environment"

  environment  = "production"
  location     = var.hcloud_location
  network_cidr = "10.20.0.0/16"
  subnet_cidr  = "10.20.1.0/24"

  with_execution_node = true

  cloudflare_account_id  = var.cloudflare_account_id
  cloudflare_zone_id     = var.cloudflare_zone_id
  hostname               = var.hostname
  ssh_hostname           = var.ssh_hostname
  ssh_hostname_execution = var.ssh_hostname_execution

  access_allowed_emails = var.access_allowed_emails

  ai_monthly_budget = var.ai_monthly_budget
  ai_budget_shares  = var.ai_budget_shares
  admin_ssh_key_ids = var.admin_ssh_key_ids
  admin_ssh_cidrs   = var.admin_ssh_cidrs
}
