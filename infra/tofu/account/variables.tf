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

variable "google_workspace_client_id" {
  description = <<-EOT
    OAuth client ID for the Google Workspace identity provider.

    Not a credential on its own — it appears in OAuth redirect URLs — so it is
    committed with the rest of the environment configuration. The paired secret
    is not.
  EOT
  type        = string
  default     = ""
}

variable "google_workspace_client_secret" {
  description = <<-EOT
    OAuth client secret for the Google Workspace identity provider.

    A credential. Supplied through TF_VAR_google_workspace_client_secret and
    never written to a tfvars file; scripts/check_infra.py fails the build if it
    ever appears in one.
  EOT
  type        = string
  default     = ""
  sensitive   = true
}

variable "google_workspace_domain" {
  description = "The Google Workspace domain whose users may authenticate, e.g. datopian.com."
  type        = string
  default     = ""
}
