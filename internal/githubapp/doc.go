// Package githubapp is delivered by WP-D1.
//
// It mints short-lived, repository-scoped GitHub App installation tokens, validates X-Hub-Signature-256 in constant time, deduplicates deliveries by GitHub delivery ID, and projects pull request and CI state. Tokens are never written into remotes, logs, or Beads.
package githubapp
