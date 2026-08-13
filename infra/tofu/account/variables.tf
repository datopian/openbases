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
  description = <<-EOT
    How long an inactive user keeps their seat before it is reclaimed.

    Also a leaver control: a seat that expires is one fewer stale identity. The
    minimum Cloudflare accepts is 730h (one month), and it is set to exactly that
    because sooner is stricter. A shorter value is rejected with
    access.api.error.user_seat_expiration_invalid_time, which does not mention
    the minimum.
  EOT
  type        = string
  default     = "730h"

  validation {
    condition     = can(regex("^[0-9]+h$", var.user_seat_expiration_inactive_time)) && tonumber(trimsuffix(var.user_seat_expiration_inactive_time, "h")) >= 730
    error_message = "user_seat_expiration_inactive_time must be expressed in hours and be at least 730h; Cloudflare rejects anything shorter."
  }
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
