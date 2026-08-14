package githubapp

import (
	"encoding/json"
	"testing"
	"time"
)

func prEvent(t *testing.T, state string, merged bool, updated time.Time) []byte {
	t.Helper()
	m := map[string]any{
		"action": "synchronize",
		"pull_request": map[string]any{
			"number": 7, "title": "A change", "state": state, "merged": merged,
			"updated_at": updated.Format(time.RFC3339),
			"user":       map[string]any{"login": "someone"},
			"head":       map[string]any{"sha": "abc123"},
		},
		"repository": map[string]any{"full_name": "datopian/portaljs"},
	}
	if merged {
		m["pull_request"].(map[string]any)["merged_at"] = updated.Format(time.RFC3339)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// GitHub reports state as only open or closed; whether a pull request merged is
// a separate boolean. Projecting state alone records every merge as a plain
// close and erases the difference between work that landed and work abandoned.
func TestMergedIsNotJustClosed(t *testing.T) {
	now := time.Now().UTC()

	var merged pullRequestEvent
	if err := json.Unmarshal(prEvent(t, "closed", true, now), &merged); err != nil {
		t.Fatal(err)
	}
	if got := merged.projectedState(); got != "merged" {
		t.Errorf("a merged pull request projected as %q, losing the fact that it landed", got)
	}

	var abandoned pullRequestEvent
	if err := json.Unmarshal(prEvent(t, "closed", false, now), &abandoned); err != nil {
		t.Fatal(err)
	}
	if got := abandoned.projectedState(); got != "closed" {
		t.Errorf("an abandoned pull request projected as %q", got)
	}

	var open pullRequestEvent
	if err := json.Unmarshal(prEvent(t, "open", false, now), &open); err != nil {
		t.Fatal(err)
	}
	if got := open.projectedState(); got != "open" {
		t.Errorf("an open pull request projected as %q", got)
	}
}

// A pull request merged_at without merged=true still means merged. GitHub sets
// the flag on some payloads and only the timestamp on others.
func TestMergedAtAloneCountsAsMerged(t *testing.T) {
	at := time.Now().UTC()
	var e pullRequestEvent
	e.PullRequest.State = "closed"
	e.PullRequest.MergedAt = &at
	if got := e.projectedState(); got != "merged" {
		t.Errorf("merged_at set but projected %q", got)
	}
}

// GitHub leaves the previous conclusion in place while a re-run is in flight,
// so reading conclusion alone reports the OLD result as the current one.
func TestInFlightRerunIsPendingNotItsOldResult(t *testing.T) {
	var e checkSuiteEvent
	e.CheckSuite.Status = "in_progress"
	e.CheckSuite.Conclusion = "success" // stale, from the previous run
	if got := e.checksState(); got != "pending" {
		t.Errorf("a re-run in flight reported %q, which is the previous run's result", got)
	}
}

func TestChecksStateMapping(t *testing.T) {
	for _, c := range []struct{ conclusion, want string }{
		{"success", "success"},
		{"failure", "failure"},
		{"timed_out", "failure"},
		{"action_required", "failure"},
		{"startup_failure", "failure"},
		{"cancelled", "neutral"},
		{"skipped", "neutral"},
		{"neutral", "neutral"},
		{"stale", "neutral"},
	} {
		var e checkSuiteEvent
		e.CheckSuite.Status = "completed"
		e.CheckSuite.Conclusion = c.conclusion
		if got := e.checksState(); got != c.want {
			t.Errorf("conclusion %q projected as %q, want %q", c.conclusion, got, c.want)
		}
	}
}

func TestMalformedPayloadsAreRefused(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not json", "{"},
		{"no number", `{"repository":{"full_name":"a/b"},"pull_request":{}}`},
		{"no repository", `{"pull_request":{"number":1}}`},
	} {
		if _, err := ProjectPullRequest(t.Context(), nil, []byte(tc.body)); err == nil {
			t.Errorf("%s: a malformed payload was accepted", tc.name)
		}
	}
	for _, tc := range []struct{ name, body string }{
		{"not json", "{"},
		{"no head sha", `{"repository":{"full_name":"a/b"},"check_suite":{}}`},
	} {
		if _, err := ProjectCheckSuite(t.Context(), nil, []byte(tc.body)); err == nil {
			t.Errorf("%s: a malformed payload was accepted", tc.name)
		}
	}
}
