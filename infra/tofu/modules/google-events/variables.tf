variable "project_id" {
  description = "Google Cloud project used SOLELY as the Pub/Sub delivery fabric for Workspace Events. No application hosting runs in Google Cloud (plan section 1.3)."
  type        = string
}

variable "region" {
  description = "Region for Pub/Sub resources."
  type        = string
  default     = "europe-west1"
}

variable "environment" {
  description = "Deployment environment, used for resource naming."
  type        = string
}

variable "push_endpoint" {
  description = "HTTPS endpoint that receives Pub/Sub push deliveries: the control API behind Cloudflare Access."
  type        = string
}

variable "ack_deadline_seconds" {
  description = "Pub/Sub acknowledgement deadline. Webhook handling stores an immutable receipt and returns quickly, then processes in the background, so this stays short."
  type        = number
  default     = 30
}

variable "message_retention_duration" {
  description = "How long Pub/Sub retains unacknowledged messages. Long enough to survive an outage of the ingestion worker and still catch up (plan section 14.2)."
  type        = string
  default     = "604800s" # 7 days
}
