variable "hcloud_location" {
  description = "Hetzner location for every node in this environment."
  type        = string
  default     = "fsn1"
}

variable "cloudflare_account_id" {
  description = "Cloudflare account identifier."
  type        = string
}

variable "cloudflare_zone_id" {
  description = "Cloudflare zone identifier for the hostname this environment serves."
  type        = string
}

variable "hostname" {
  description = "Fully qualified hostname served through Cloudflare Access and Tunnel."
  type        = string
}

variable "access_allowed_emails" {
  description = "Emails permitted by the Cloudflare Access policy. Empty denies everyone."
  type        = list(string)
  default     = []
}

variable "admin_ssh_key_ids" {
  description = "Hetzner SSH key IDs placed on the nodes for console break-glass."
  type        = list(string)
  default     = []
}

variable "admin_ssh_cidrs" {
  description = "Source CIDRs allowed to reach SSH. Leave empty; setting it opens a public inbound port."
  type        = list(string)
  default     = []
}

variable "ssh_hostname" {
  description = "Hostname for SSH over the tunnel, used by configuration management. Opens no port."
  type        = string
  default     = ""
}

variable "execution_server_type" {
  description = <<-EOT
    Hetzner type for the staging execution node.

    Deliberately far smaller than production. Staging proves isolation — user
    separation, home permissions, cgroup limits, blocked metadata — and none of
    those assertions are memory-bound. Production sizes for 5-8 concurrent
    build-heavy agents (plan section 11.1); staging does not run them.
  EOT
  type        = string
  default     = "cx23"
}

variable "ssh_hostname_execution" {
  description = "Hostname for SSH to the execution node, over its own tunnel."
  type        = string
  default     = ""
}

variable "ai_monthly_budget" {
  type        = number
  description = <<-EOT
    Shared monthly agent spend ceiling in US dollars, applied to EACH gateway as
    a platform backstop. Cloudflare enforces a limit per gateway and cannot
    express one pool shared across three; the real shared pool is summed in the
    control plane. Zero disables agent spend, which is the correct default: an
    unset budget must fail closed.
  EOT
  default     = 0
}
