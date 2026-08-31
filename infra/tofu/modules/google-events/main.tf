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
    # Reading or setting a service account's IAM policy goes through this API,
    # and it is NOT in the bundle Google pre-enables on a new project. Without
    # it the token-creator grant below fails with accessNotConfigured on
    # "Error retrieving IAM policy for service account" — which names IAM, the
    # service account, and the project number, and does not obviously mean "an
    # API is off".
    #
    # Second instance of the same shape as the cloudresourcemanager note above:
    # a call that manages permissions needs its own API on first, and the error
    # arrives from the resource that wanted it rather than from anything about
    # enablement.
    "iam.googleapis.com",
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

# The project's number, for the Pub/Sub service agent's address.
#
# A data source rather than a variable: the number is derived from the project
# id, and asking a human to copy it into tfvars is one more place for the two to
# disagree.
data "google_project" "this" {
  project_id = var.project_id
}

# Pub/Sub cannot mint an OIDC token unless its own service agent is allowed to
# act as the identity the token is signed as.
#
# This is the grant that makes push authentication work at all, and its absence
# is invisible from our side: the subscription accepts the oidc_token block,
# Google then fails to create a token for each delivery, and NOTHING arrives at
# the endpoint — no request, no refusal, no log line. Which reads exactly like
# "no events have happened yet".
#
# The member is Google's own managed agent, created when the Pub/Sub API is
# enabled. It is not an identity we control and it can do nothing else with this
# grant: getOpenIdToken mints a token whose audience is fixed by the
# subscription.
resource "google_service_account_iam_member" "pubsub_token_creator" {
  service_account_id = "projects/${var.project_id}/serviceAccounts/${var.push_service_account}"
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = "serviceAccount:service-${data.google_project.this.number}@gcp-sa-pubsub.iam.gserviceaccount.com"

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

    # Google signs a JWT for every delivery and the handler verifies it
    # (ADR-0026). Without this the endpoint's only protection is that its URL is
    # not widely known, which is the position the GitHub webhook is in and the
    # one this deliberately does not repeat.
    #
    # The audience is the endpoint itself. A token minted for another
    # Google-fronted service then fails here, which is the difference between
    # proving a request came from Google and proving it was meant for us.
    oidc_token {
      service_account_email = var.push_service_account
      audience              = var.push_endpoint
    }
  }

  dead_letter_policy {
    dead_letter_topic     = google_pubsub_topic.dead_letter.id
    max_delivery_attempts = 10
  }

  retry_policy {
    minimum_backoff = "10s"
    maximum_backoff = "600s"
  }

  # Without the grant, Google refuses a push subscription that names an identity
  # its service agent cannot act as — and on an existing subscription it accepts
  # the update and then silently delivers nothing.
  depends_on = [google_service_account_iam_member.pubsub_token_creator]

  expiration_policy {
    # Never expire the subscription itself. A silently expired subscription is
    # exactly the failure mode that loses meetings without anyone noticing.
    ttl = ""
  }
}
