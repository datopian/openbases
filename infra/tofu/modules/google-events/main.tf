# Google Workspace Events delivery fabric.
#
# Workspace Events subscriptions deliver through Pub/Sub, so a minimal Google
# Cloud project is required (plan section 6.1). It hosts nothing else.
#
# Subscriptions themselves are created by the connector at runtime, not here:
# they expire and must be renewed, tracked and reconciled, which is application
# behaviour (WP-H1), not infrastructure.

locals {
  name = "workgraph-workspace-events-${var.environment}"
}

# PREREQUISITE, and it cannot be met from here: cloudresourcemanager.googleapis.com
# must already be enabled on the project.
#
# google_project_service enables an API by calling the Cloud Resource Manager
# API, so Terraform cannot turn on the thing it needs in order to turn things
# on. Without it a plan does not fail cleanly either — it reports
# "0 to add, 0 to change" with the real error buried in refresh output, which is
# how this looked like a module that had nothing left to do.
#
#   gcloud services enable cloudresourcemanager.googleapis.com --project <ID>
resource "google_project_service" "required" {
  for_each = toset([
    "pubsub.googleapis.com",
    "workspaceevents.googleapis.com",
    "meet.googleapis.com",
    "drive.googleapis.com",
  ])

  project = var.project_id
  service = each.value

  # Disabling an API on destroy would break ingestion for anything else using
  # the project, and these are cheap to leave enabled.
  disable_on_destroy = false
}

resource "google_pubsub_topic" "workspace_events" {
  project = var.project_id
  name    = local.name

  message_retention_duration = var.message_retention_duration

  labels = {
    project     = "workgraph"
    environment = var.environment
  }

  depends_on = [google_project_service.required]
}

# Google publishes into the topic, so its publisher needs rights on it — and
# the publisher is PER APPLICATION, not one account for Workspace Events.
#
# This was written as a single `workspace-events@system.gserviceaccount.com`,
# which does not exist. The apply failed with "Service account
# workspace-events@system.gserviceaccount.com does not exist", which reads like
# a provisioning delay for an account that has not appeared yet, and is not:
# the name was invented. Google names one per source (chat-api-push for Chat).
#
# Two, because we ingest from both Meet and Drive. Granting only the one whose
# events arrive first would look like it worked until the other source went
# quiet — and a subscription that silently delivers nothing is the failure this
# whole pipeline is least able to notice.
locals {
  event_publishers = {
    meet  = "meet-api-event-push@system.gserviceaccount.com"
    drive = "drive-api-event-push@system.gserviceaccount.com"
  }
}

resource "google_pubsub_topic_iam_member" "workspace_events_publisher" {
  for_each = local.event_publishers

  project = var.project_id
  topic   = google_pubsub_topic.workspace_events.name
  role    = "roles/pubsub.publisher"
  member  = "serviceAccount:${each.value}"
}

# Undeliverable messages go here rather than being retried forever or dropped.
# A message that cannot be processed is evidence of a bug, and it must survive
# long enough to be investigated.
resource "google_pubsub_topic" "dead_letter" {
  project = var.project_id
  name    = "${local.name}-dead-letter"

  message_retention_duration = var.message_retention_duration

  depends_on = [google_project_service.required]
}

resource "google_pubsub_subscription" "workspace_events" {
  project = var.project_id
  name    = local.name
  topic   = google_pubsub_topic.workspace_events.id

  ack_deadline_seconds       = var.ack_deadline_seconds
  message_retention_duration = var.message_retention_duration
  # Deliveries are deduplicated by message ID in the control plane, so
  # at-least-once redelivery is safe and expected.
  retain_acked_messages = false

  push_config {
    push_endpoint = var.push_endpoint
  }

  dead_letter_policy {
    dead_letter_topic     = google_pubsub_topic.dead_letter.id
    max_delivery_attempts = 10
  }

  retry_policy {
    minimum_backoff = "10s"
    maximum_backoff = "600s"
  }

  expiration_policy {
    # Never expire the subscription itself. A silently expired subscription is
    # exactly the failure mode that loses meetings without anyone noticing.
    ttl = ""
  }
}
