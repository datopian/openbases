output "hostname" {
  value = module.environment.hostname
}

output "control_node_ipv4" {
  value = module.environment.control_node_ipv4
}

output "execution_node_ipv4" {
  value = module.environment.execution_node_ipv4
}

output "control_tunnel_id" {
  value = module.environment.control_tunnel_id
}

output "execution_tunnel_id" {
  value = module.environment.execution_tunnel_id
}

output "ssh_hostname" {
  value = module.environment.ssh_hostname
}

output "ssh_hostname_execution" {
  value = module.environment.ssh_hostname_execution
}

output "r2_buckets" {
  value = module.environment.r2_buckets
}

output "cell_token_mint_aud" {
  value = module.environment.cell_token_mint_aud
}

output "cell_agent_health_aud" {
  value = module.environment.cell_agent_health_aud
}

output "cell_budget_check_aud" {
  value = module.environment.cell_budget_check_aud
}

output "probe_client_id" {
  value = module.environment.probe_client_id
}

output "probe_client_secret" {
  value     = module.environment.probe_client_secret
  sensitive = true
}
