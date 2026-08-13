output "topic" {
  description = "Fully qualified Pub/Sub topic that Workspace Events publishes into."
  value       = google_pubsub_topic.workspace_events.id
}

output "topic_name" {
  description = "Short topic name, used when creating Workspace Events subscriptions."
  value       = google_pubsub_topic.workspace_events.name
}

output "subscription" {
  description = "Push subscription delivering to the control API."
  value       = google_pubsub_subscription.workspace_events.id
}

output "dead_letter_topic" {
  description = "Topic holding messages that exhausted their delivery attempts."
  value       = google_pubsub_topic.dead_letter.id
}
