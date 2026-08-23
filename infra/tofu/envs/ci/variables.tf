variable "hcloud_token" {
  description = "Token for the Github Actions Runner Hetzner project. NOT the platform project."
  type        = string
  sensitive   = true
}

variable "cloudflare_api_token" {
  description = "Cloudflare token, for the management tunnel and its DNS record."
  type        = string
  sensitive   = true
}

variable "cloudflare_account_id" {
  type = string
}

variable "cloudflare_zone_id" {
  type = string
}

variable "ssh_hostname" {
  description = "Management hostname over the tunnel."
  type        = string
  default     = "ssh-ci-runner.openbases.com"
}

variable "runner_server_type" {
  description = "cx33: 4 vCPU / 8 GB, EUR 8.49/month. See docs/ci-runners.md for the sizing."
  type        = string
  default     = "cx33"
}

variable "location" {
  type    = string
  default = "fsn1"
}

variable "admin_ssh_key_ids" {
  description = "Break-glass only, through the Hetzner console. No public SSH port is opened."
  type        = list(string)
  default     = []
}

# Same pin as the Workgraph environments, deliberately: one connector version to
# reason about across every node we run, and one place to bump it.
variable "cloudflared_version" {
  type    = string
  default = "2026.7.3"
}

variable "cloudflared_sha256" {
  type    = string
  default = "049777d30f9bf93da6df8bbe31383460eb2aa51a832c6551824d56f9fcc55974"
}
