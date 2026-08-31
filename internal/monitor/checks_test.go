package monitor

import (
	"strings"
	"testing"
	"time"
)

// A lapsed Workspace subscription is the failure the WP-H1 timer exists to
// prevent, and it is invisible without this check: the source simply stops
// delivering. Nothing errors, nothing is slow, and the missed events do not
// accumulate anywhere — they are never sent.
func TestALapsedWorkspaceSubscriptionIsReported(t *testing.T) {
	at := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	th := DefaultThresholds()

	healthy := []WorkspaceSource{
		{Name: "All", Kind: "drive", Enabled: true, State: "active", ExpiresAt: at.Add(7 * 24 * time.Hour)},
		{Name: "Innovation Team Sync", Kind: "meet", Enabled: true, State: "active", ExpiresAt: at.Add(5 * 24 * time.Hour)},
	}
	if f := EvaluateWorkspaceSources(healthy, at, th); f.Failing {
		t.Errorf("healthy subscriptions reported failing: %s", f.Summary)
	}

	for name, s := range map[string]WorkspaceSource{
		"no subscription": {Name: "All", Enabled: true, State: "pending"},
		"suspended":       {Name: "All", Enabled: true, State: "suspended", ExpiresAt: at.Add(48 * time.Hour)},
		"failed":          {Name: "All", Enabled: true, State: "failed", LastError: "google returned 403: {\n  \"error\": {"},
		"already expired": {Name: "All", Enabled: true, State: "active", ExpiresAt: at.Add(-time.Hour)},
		"no expiry":       {Name: "All", Enabled: true, State: "active"},
		"not renewed": {Name: "All", Enabled: true, State: "active",
			ExpiresAt: at.Add(th.SubscriptionRenewalGrace - time.Minute)},
	} {
		f := EvaluateWorkspaceSources([]WorkspaceSource{s}, at, th)
		if !f.Failing {
			t.Errorf("%s was reported healthy", name)
		}
		if !strings.Contains(f.Summary, "All") {
			t.Errorf("%s: summary does not name the source: %q", name, f.Summary)
		}
	}

	// A multi-line Google error must not be pasted whole into a summary.
	f := EvaluateWorkspaceSources([]WorkspaceSource{
		{Name: "All", Enabled: true, State: "failed",
			LastError: "google returned 403: {\n  \"error\": {\n    \"code\": 403"},
	}, at, th)
	if strings.Contains(f.Summary, "\n") {
		t.Errorf("the summary carries a multi-line error: %q", f.Summary)
	}
}

// Delivery silence is NOT a failure. A Meet source produces nothing for days —
// the meeting is Mon-Thu and is sometimes skipped or held with transcription off
// — and a Drive source is quiet at weekends. An alert that fires during correct
// operation is one people mute in week one.
func TestWorkspaceSilenceIsNotAFailure(t *testing.T) {
	at := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	f := EvaluateWorkspaceSources([]WorkspaceSource{
		{Name: "Innovation Team Sync", Kind: "meet", Enabled: true, State: "active",
			ExpiresAt: at.Add(6 * 24 * time.Hour)},
	}, at, DefaultThresholds())
	if f.Failing {
		t.Errorf("a source that has delivered nothing was reported failing: %s", f.Summary)
	}
}

// An empty allow-list must fail rather than read as "nothing to report", the
// same reasoning as declared backup streams. The likely cause is migrations not
// applied or the table read through a path RLS hides.
func TestNoWorkspaceSourceAtAllIsAFailure(t *testing.T) {
	at := time.Now()
	if f := EvaluateWorkspaceSources(nil, at, DefaultThresholds()); !f.Failing {
		t.Error("an empty allow-list was reported healthy")
	}
	if f := EvaluateWorkspaceSources([]WorkspaceSource{
		{Name: "All", Enabled: false},
	}, at, DefaultThresholds()); !f.Failing {
		t.Error("every source disabled was reported healthy")
	}
}

// The threshold must be shorter than the reconciler's own renewal window, or the
// alert fires during correct operation: every subscription passes through the
// renewal window on its way to being renewed.
func TestTheRenewalGraceLeavesTheReconcilerRoomToWork(t *testing.T) {
	const renewBefore = 24 * time.Hour // workspace.DefaultPolicy.RenewBefore
	if g := DefaultThresholds().SubscriptionRenewalGrace; g >= renewBefore {
		t.Errorf("grace %s is not shorter than the reconciler's %s renewal window, so the "+
			"alert fires every time a subscription is due for renewal", g, renewBefore)
	}
}
