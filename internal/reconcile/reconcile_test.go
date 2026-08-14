package reconcile

import (
	"encoding/json"
	"testing"
	"time"
)

// The API response must render into the SAME payload shape the webhook
// projection consumes.
//
// If it did not, resync would need its own projection path, and two paths that
// must agree on merged-versus-closed and on the ordering guard would eventually
// stop agreeing — with the divergence appearing only after an outage, when the
// resync path finally runs.
func TestAPIResponseRendersAsAWebhookPayload(t *testing.T) {
	merged := time.Date(2026, 8, 14, 15, 0, 0, 0, time.UTC)
	pr := apiPullRequest{
		Number: 12, Title: "A change", State: "closed",
		Merged: true, MergedAt: &merged, UpdatedAt: merged,
	}
	pr.User.Login = "someone"
	pr.Head.SHA = "abc123"

	payload, err := asWebhookPayload(pr, "123456", "datopian/portaljs")
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		PullRequest struct {
			Number    int        `json:"number"`
			State     string     `json:"state"`
			Merged    bool       `json:"merged"`
			MergedAt  *time.Time `json:"merged_at"`
			UpdatedAt time.Time  `json:"updated_at"`
			User      struct {
				Login string `json:"login"`
			} `json:"user"`
			Head struct {
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		Repository struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("the rendered payload does not parse as a webhook payload: %v", err)
	}

	if got.PullRequest.Number != 12 || got.PullRequest.Head.SHA != "abc123" {
		t.Errorf("identity fields lost: %+v", got.PullRequest)
	}
	// merged and merged_at are what distinguish a merge from a plain close.
	if !got.PullRequest.Merged || got.PullRequest.MergedAt == nil {
		t.Error("the merge was lost in translation; resync would record it as closed")
	}
	// updated_at drives the ordering guard. Losing it makes every resynced row
	// look older than everything, so nothing would ever apply.
	if got.PullRequest.UpdatedAt.IsZero() {
		t.Error("updated_at was lost; the ordering guard would reject every resync")
	}
	if got.PullRequest.User.Login != "someone" {
		t.Error("author was lost")
	}
	if got.Repository.ID != 123456 || got.Repository.FullName != "datopian/portaljs" {
		t.Errorf("repository identity lost: %+v", got.Repository)
	}
}

// A repository whose stable ID has not been recorded yet must still resync,
// falling back to owner and name.
func TestPayloadWithoutAProviderIDStillIdentifiesTheRepository(t *testing.T) {
	payload, err := asWebhookPayload(apiPullRequest{Number: 1}, "", "datopian/portaljs")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Repository map[string]any `json:"repository"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if _, present := got.Repository["id"]; present {
		t.Error("an empty provider ID must be omitted, not sent as a literal zero")
	}
	if got.Repository["full_name"] != "datopian/portaljs" {
		t.Error("the fallback identity is missing")
	}
}

func TestResultLogArgsArePaired(t *testing.T) {
	args := Result{Replayed: 1}.LogArgs()
	if len(args)%2 != 0 {
		t.Fatalf("slog needs key/value pairs, got %d args", len(args))
	}
}
