output "hostname" {
  value = module.environment.hostname
}

output "control_node_ipv4" {
  value = module.environment.control_node_ipv4
}

output "execution_node_ipv4" {
  value = module.environment.execution_node_ipv4
}

output "tunnel_id" {
  value = module.environment.tunnel_id
}

output "r2_buckets" {
  value = module.environment.r2_buckets
}
