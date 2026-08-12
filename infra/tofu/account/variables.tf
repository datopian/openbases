variable "cloudflare_account_id" {
  description = "Cloudflare account identifier."
  type        = string
}

variable "team_name" {
  description = <<-EOT
    The Zero Trust team domain label, becoming <team_name>.cloudflareaccess.com.

    This is externally visible on every login screen and is the JWT issuer the
    control API validates against. Changing it later invalidates enrolled
    devices, registered identity-provider callback URLs, and every bookmark, so
    it is cheap to set now and expensive to revisit.
  EOT
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$", var.team_name))
    error_message = "team_name must be a lowercase DNS label."
  }
}

variable "display_name" {
  description = "Human-readable organisation name shown on the login page."
  type        = string
  default     = "Datopian"
}

variable "session_duration" {
  description = "Default Access session lifetime before re-authentication."
  type        = string
  default     = "24h"
}

variable "user_seat_expiration_inactive_time" {
  description = "How long an inactive user keeps their seat. Reclaiming seats is also a leaver control."
  type        = string
  default     = "720h" # 30 days
}
