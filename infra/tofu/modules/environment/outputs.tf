output "control_node_ipv4" {
  description = "Public IPv4 of the control node. Present for administrative reference; no inbound service listens on it."
  value       = hcloud_server.control.ipv4_address
}

output "control_node_private_ip" {
  description = "Private network address of the control node."
  value       = tolist(hcloud_server.control.network)[0].ip
}

output "execution_node_ipv4" {
  description = "Public IPv4 of the execution node, when one exists."
  value       = var.with_execution_node ? hcloud_server.execution[0].ipv4_address : null
}

output "execution_node_private_ip" {
  description = "Private network address of the execution node, when one exists."
  value       = var.with_execution_node ? tolist(hcloud_server.execution[0].network)[0].ip : null
}

output "control_tunnel_id" {
  description = "Cloudflare Tunnel serving the control node."
  value       = cloudflare_zero_trust_tunnel_cloudflared.control.id
}

output "execution_tunnel_id" {
  description = "Cloudflare Tunnel serving the execution node, when one exists."
  value       = try(cloudflare_zero_trust_tunnel_cloudflared.execution[0].id, null)
}


output "hostname" {
  description = "The hostname served by this environment."
  value       = var.hostname
}

output "r2_buckets" {
  description = "Names of the R2 buckets created for this environment."
  value       = [for b in cloudflare_r2_bucket.this : b.name]
}

output "firewall_id" {
  description = "The deny-by-default firewall applied to every node."
  value       = hcloud_firewall.this.id
}

output "ssh_hostname" {
  description = "Hostname for SSH to the control node."
  value       = var.ssh_hostname
}

output "ssh_hostname_execution" {
  description = "Hostname for SSH to the execution node."
  value       = var.with_execution_node ? var.ssh_hostname_execution : null
}

output "ai_gateway_ids" {
  description = "AI Gateway identifiers by security domain."
  value       = { for k, g in cloudflare_ai_gateway.cell : k => g.id }
}

output "ai_gateway_urls" {
  description = "Anthropic-compatible endpoint per security domain, for ANTHROPIC_BASE_URL."
  value = {
    for k, g in cloudflare_ai_gateway.cell :
    k => "https://gateway.ai.cloudflare.com/v1/${var.cloudflare_account_id}/${g.id}/anthropic"
  }
}

# The audiences the control API must accept, so that wiring the service does not
# depend on reading them out of the dashboard. Not secrets: an AUD identifies an
# application, it does not grant anything.
output "cell_token_mint_aud" {
  description = "AUD of the Access application fronting the git-credential endpoint."
  value       = cloudflare_zero_trust_access_application.cell_token_mint.aud
}

output "cell_agent_health_aud" {
  description = "AUD of the Access application fronting the agent-health endpoint (ADR-0019)."
  value       = cloudflare_zero_trust_access_application.cell_agent_health.aud
}

output "cell_budget_check_aud" {
  description = "AUD of the Access application fronting the budget-check endpoint (wg-qw1)."
  value       = cloudflare_zero_trust_access_application.cell_budget_check.aud
}
