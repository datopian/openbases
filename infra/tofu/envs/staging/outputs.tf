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
