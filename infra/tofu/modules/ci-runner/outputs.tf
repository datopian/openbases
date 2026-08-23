output "runner_ipv4" {
  description = "Public address. Nothing may connect to it; useful for egress allow-lists elsewhere."
  value       = hcloud_server.runner.ipv4_address
}

output "ssh_hostname" {
  description = "Reach the runner with: ssh -o ProxyCommand=\"cloudflared access ssh --hostname %h\" root@<this>"
  value       = var.ssh_hostname
}

output "tunnel_id" {
  value = cloudflare_zero_trust_tunnel_cloudflared.this.id
}
