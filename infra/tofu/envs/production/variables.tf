# Staging variables. Values live in a tfvars file supplied by the pipeline, or
# in environment variables. Secret values are never defaulted here.

variable "environment" {
  description = "Deployment environment name."
  type        = string
  default     = "production"

  validation {
    condition     = contains(["staging", "production"], var.environment)
    error_message = "environment must be staging or production."
  }
}

variable "hcloud_location" {
  description = "Hetzner location for all nodes in this environment."
  type        = string
  default     = "hel1"
}

variable "control_node_type" {
  description = "Hetzner server type for the control node (API, PostgreSQL, worker, observability)."
  type        = string
  default     = "cx32"
}

variable "cloudflare_account_id" {
  description = "Cloudflare account identifier."
  type        = string
}

variable "cloudflare_zone_id" {
  description = "Cloudflare zone identifier for the hostname served in this environment."
  type        = string
}

variable "hostname" {
  description = "Public hostname served through Cloudflare Access and Tunnel."
  type        = string
  default     = "work.datopian.com"
}

variable "google_project_id" {
  description = "Google Cloud project used solely for Workspace Events delivery through Pub/Sub."
  type        = string
}

variable "google_region" {
  description = "Google Cloud region for Pub/Sub resources."
  type        = string
  default     = "europe-west1"
}

variable "access_allowed_emails" {
  description = "Email addresses permitted by the Cloudflare Access policy for this environment."
  type        = list(string)
  default     = []
}
