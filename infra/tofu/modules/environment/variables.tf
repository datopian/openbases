variable "environment" {
  description = "Deployment environment. Drives naming, sizing and firewall posture."
  type        = string

  validation {
    condition     = contains(["staging", "production"], var.environment)
    error_message = "environment must be staging or production."
  }
}

variable "location" {
  description = "Hetzner location. All nodes in an environment share one location: placement groups do not span locations."
  type        = string
  default     = "fsn1"
}

variable "network_cidr" {
  description = "Private network CIDR. Staging and production use different ranges so a misrouted peer cannot reach the wrong environment."
  type        = string
}

variable "subnet_cidr" {
  description = "Subnet within network_cidr."
  type        = string
}

variable "control_server_type" {
  description = "Hetzner server type for the control node: API, PostgreSQL, worker, observability. Never runs agent worktrees."
  type        = string
  default     = "cx43"
}

variable "execution_server_type" {
  description = "Hetzner server type for the execution node: Gas Town cells, Dolt, worktrees, builds. Production only."
  type        = string
  default     = "cx53"
}

variable "image" {
  description = "Base OS image. Ubuntu LTS."
  type        = string
  default     = "ubuntu-24.04"
}

variable "with_execution_node" {
  description = "Whether to create a separate execution node. Production runs the control plane and agent workloads on different machines so a noisy build cannot starve the system of record. Staging runs a single node."
  type        = bool
  default     = false
}

variable "enable_hetzner_backups" {
  description = "Hetzner daily server backups, an additional disaster-recovery layer. Live backups may be inconsistent and exclude volumes, so this never replaces database-native backup."
  type        = bool
  default     = true
}

variable "admin_ssh_key_ids" {
  description = "Hetzner SSH key IDs installed on the nodes. Used only for break-glass through the Hetzner console; inbound SSH from the internet stays closed."
  type        = list(string)
  default     = []
}

variable "admin_ssh_cidrs" {
  description = <<-EOT
    Source CIDRs permitted to reach SSH.

    Empty by default, and it should stay empty: the origin is reached through
    Cloudflare Tunnel, which connects outbound. Setting this opens a public
    inbound port and must be a deliberate, reviewed, temporary act — it will be
    visible in the plan diff, which is the point.
  EOT
  type        = list(string)
  default     = []
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
  description = "Email addresses permitted by the Cloudflare Access policy. An empty list denies everyone, which is the correct failure direction."
  type        = list(string)
  default     = []
}

variable "access_session_duration" {
  description = "Cloudflare Access session lifetime."
  type        = string
  default     = "24h"
}

variable "r2_buckets" {
  description = <<-EOT
    R2 bucket base names created for this environment. The environment name is
    appended.

    "tfstate" is deliberately absent. The state bucket cannot be created by the
    configuration whose state it holds, so scripts/bootstrap_state_bucket.sh
    creates it before the first init. Listing it here as well would mean two
    owners for one bucket, and the apply would fail on a name that already
    exists.
  EOT
  type        = list(string)
  default     = ["backups", "evidence", "audit"]

  validation {
    condition     = !contains(var.r2_buckets, "tfstate")
    error_message = "tfstate is owned by scripts/bootstrap_state_bucket.sh, not by this module."
  }
}

variable "r2_location" {
  description = "R2 bucket location hint. Western Europe keeps client-derived evidence near the Falkenstein nodes and inside the EEA."
  type        = string
  default     = "weur"

  validation {
    condition     = contains(["weur", "eeur", "enam", "wnam", "apac", "oc"], var.r2_location)
    error_message = "r2_location must be one of weur, eeur, enam, wnam, apac, oc."
  }
}

variable "cloudflared_version" {
  description = "Pinned cloudflared release. Bootstrapped by cloud-init and verified against cloudflared_sha256."
  type        = string
  default     = "2026.7.3"
}

variable "cloudflared_sha256" {
  description = "SHA-256 of cloudflared-linux-amd64.deb for cloudflared_version. A mismatch aborts the bootstrap."
  type        = string
  default     = "049777d30f9bf93da6df8bbe31383460eb2aa51a832c6551824d56f9fcc55974"
}

variable "app_port" {
  description = "Local port the tunnel routes to on the control node. The control API binds here."
  type        = number
  default     = 8080
}
