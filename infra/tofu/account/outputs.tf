output "team_domain" {
  description = "The Zero Trust team domain. This is WG_ACCESS_TEAM_DOMAIN for the control API."
  value       = cloudflare_zero_trust_organization.this.auth_domain
}

output "auth_domain_label" {
  description = "The team domain label alone."
  value       = var.team_name
}

output "identity_providers" {
  description = "Configured identity providers. Empty until Google Workspace credentials are supplied."
  value       = [for idp in cloudflare_zero_trust_access_identity_provider.google_workspace : idp.name]
}
