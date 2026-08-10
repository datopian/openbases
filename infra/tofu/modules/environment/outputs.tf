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

output "tunnel_id" {
  description = "Cloudflare Tunnel identifier."
  value       = cloudflare_zero_trust_tunnel_cloudflared.this.id
}

output "tunnel_cname" {
  description = "The tunnel's CNAME target."
  value       = "${cloudflare_zero_trust_tunnel_cloudflared.this.id}.cfargotunnel.com"
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
