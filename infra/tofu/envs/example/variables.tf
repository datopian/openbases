# Copied from envs/staging. See main.tf in this directory.

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

variable "api_hostname" {
  description = "Fully qualified hostname for the token-authenticated API surface, or empty for none. The module variable explains why it is a separate name (wg-p4h.1)."
  type        = string
  default     = ""
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
    Hetzner type for the execution node, where agents run.

    The default is small on purpose: it is enough to prove the isolation model
    — user separation, home permissions, cgroup limits, blocked metadata — none
    of which is memory-bound. Size up before running build-heavy agents; a
    `npm ci` on a large repository has been observed needing more than 1.7 GB
    and being OOM-killed at less.
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

variable "ai_budget_shares" {
  type        = map(number)
  description = "How the shared monthly pool is divided between security domains. Must sum to 1."
  default     = {}
}

variable "ai_gateway_store_id" {
  description = <<-EOT
    The AI Gateway log store id, assigned by Cloudflare.

    NO DEFAULT here on purpose. envs/staging carries ours as a default so that
    plans stop proposing to remove it — see the module variable for why that
    removal was dangerous rather than cosmetic — and inheriting that value
    would point your environment at our store. Create the gateway, then read
    the id back and set it.
  EOT
  type        = string
}

variable "google_project_id" {
  description = "Google Cloud project used SOLELY as the Pub/Sub delivery fabric for Workspace Events. Empty disables the module entirely (wg-8yv.34)."
  type        = string
  default     = ""
}

variable "google_region" {
  description = "Region for Pub/Sub resources."
  type        = string
  default     = "europe-west1"
}
