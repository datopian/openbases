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
	if f := EvaluateWorkspaceSources(healthy, true, at, th); f.Failing {
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
		f := EvaluateWorkspaceSources([]WorkspaceSource{s}, true, at, th)
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
	}, true, at, th)
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
	}, true, at, DefaultThresholds())
	if f.Failing {
		t.Errorf("a source that has delivered nothing was reported failing: %s", f.Summary)
	}
}

// An empty allow-list must fail rather than read as "nothing to report", the
// same reasoning as declared backup streams. The likely cause is migrations not
// applied or the table read through a path RLS hides.
func TestNoWorkspaceSourceAtAllIsAFailure(t *testing.T) {
	at := time.Now()
	if f := EvaluateWorkspaceSources(nil, true, at, DefaultThresholds()); !f.Failing {
		t.Error("an empty allow-list was reported healthy")
	}
	if f := EvaluateWorkspaceSources([]WorkspaceSource{
		{Name: "All", Enabled: false},
	}, true, at, DefaultThresholds()); !f.Failing {
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

// The graph check exists because of a failure nothing else could see: the files
// were present, intact, and root-owned, so the graph looked healthy to every
// check that ran as root. Each case below is that failure or a way of
// accidentally hiding it again.
func TestAnUnreadableGraphIsAFailure(t *testing.T) {
	f := EvaluateGraphs([]Graph{
		{Name: "company-hq", Path: "/srv/graphs/company-hq", Readable: false,
			Reason: "workgraph cannot read /srv/graphs/company-hq/.beads/.../manifest"},
	})
	if !f.Failing {
		t.Fatal("a graph the service user cannot read must fail: the work has stopped")
	}
	if !strings.Contains(f.Summary, "company-hq") {
		t.Errorf("the summary should name the graph, got %q", f.Summary)
	}
	if !strings.Contains(f.Summary, "service user") {
		t.Errorf("the summary should say who cannot read it, got %q", f.Summary)
	}
}

func TestAMissingGraphIsAFailureAndSaysSoDifferently(t *testing.T) {
	f := EvaluateGraphs([]Graph{
		{Name: "company-hq", Path: "/srv/graphs/company-hq", Missing: true},
	})
	if !f.Failing {
		t.Fatal("a graph that is not on disk must fail")
	}
	// Missing and unreadable have different fixes -- restore versus chown --
	// so the report must not collapse them.
	if !strings.Contains(f.Summary, "not on disk") {
		t.Errorf("a missing graph should say so rather than reading as a permission problem, got %q",
			f.Summary)
	}
}

func TestAReadableGraphPasses(t *testing.T) {
	f := EvaluateGraphs([]Graph{
		{Name: "company-hq", Path: "/srv/graphs/company-hq", Readable: true},
		{Name: "cell-oss", Path: "/srv/graphs/cell-oss", Readable: true},
	})
	if f.Failing {
		t.Fatalf("two readable graphs should pass, got %q", f.Summary)
	}
	if !strings.Contains(f.Summary, "2") {
		t.Errorf("the summary should say how many were checked, got %q", f.Summary)
	}
}

// Declaring nothing must not look like health. This is the same rule the backup
// check follows, and for the same reason: a check nobody declared is a check
// that is off, and "off" reported as "healthy" is how a guarantee is lost
// silently.
func TestNoGraphDeclaredAtAllIsAFailure(t *testing.T) {
	f := EvaluateGraphs(nil)
	if !f.Failing {
		t.Fatal("declaring no graph switches the check off; that must not read as healthy")
	}
	if !strings.Contains(f.Summary, "no work graph is declared") {
		t.Errorf("the summary should say the check is not configured, got %q", f.Summary)
	}
}

// One broken graph among several must still fail, and must name the broken one.
// A report that says "1 of 2 readable" and passes is worse than no report.
func TestOneBrokenGraphAmongSeveralFails(t *testing.T) {
	f := EvaluateGraphs([]Graph{
		{Name: "company-hq", Path: "/a", Readable: true},
		{Name: "cell-oss", Path: "/b", Readable: false, Reason: "permission denied"},
	})
	if !f.Failing {
		t.Fatal("one unreadable graph among several must fail the check")
	}
	if !strings.Contains(f.Summary, "cell-oss") {
		t.Errorf("the summary must name the broken graph, got %q", f.Summary)
	}
	if strings.Contains(f.Summary, "company-hq") {
		t.Errorf("the summary should not clutter itself with the healthy graph, got %q", f.Summary)
	}
}

// The observed detail is what an inbox item carries, so every declared graph
// has to appear in it whether or not it is the one that failed.
func TestEveryDeclaredGraphIsInTheObservedDetail(t *testing.T) {
	f := EvaluateGraphs([]Graph{
		{Name: "company-hq", Path: "/a", Readable: true},
		{Name: "cell-oss", Path: "/b", Readable: false, Reason: "permission denied"},
	})
	entries, ok := f.Observed["graphs"].([]map[string]any)
	if !ok {
		t.Fatalf("observed graphs should be a list of entries, got %T", f.Observed["graphs"])
	}
	if len(entries) != 2 {
		t.Fatalf("both graphs should be reported, got %d", len(entries))
	}
	states := map[string]string{}
	for _, e := range entries {
		states[e["name"].(string)] = e["state"].(string)
	}
	if states["company-hq"] != "readable" || states["cell-oss"] != "unreadable" {
		t.Errorf("states wrong: %v", states)
	}
}

// A provisioned-but-empty graph is not an outage.
//
// beads_hq creates the directory and Dolt initialises on first write, so a
// declared graph nobody has written to is normal — two of the three graphs on
// staging are in that state. The first version of this check called it a
// failure, and the very first live run reported two false outages.
func TestAnUninitialisedGraphIsReportedAndDoesNotFail(t *testing.T) {
	f := EvaluateGraphs([]Graph{
		{Name: "company-hq", Path: "/a", Readable: true},
		{Name: "project-cdt", Path: "/b", Uninitialised: true},
	})
	if f.Failing {
		t.Fatalf("an empty graph is not an outage, got %q", f.Summary)
	}
	if !strings.Contains(f.Summary, "not yet initialised") {
		t.Errorf("the summary should still mention it, got %q", f.Summary)
	}
	entries, ok := f.Observed["graphs"].([]map[string]any)
	if !ok {
		t.Fatalf("observed graphs should be entries, got %T", f.Observed["graphs"])
	}
	for _, e := range entries {
		if e["name"] == "project-cdt" && e["state"] != "uninitialised" {
			t.Errorf("an empty graph should be labelled uninitialised, got %v", e["state"])
		}
	}
}

// An uninitialised graph must not mask a broken one beside it.
func TestAnUninitialisedGraphDoesNotHideARealFailure(t *testing.T) {
	f := EvaluateGraphs([]Graph{
		{Name: "project-cdt", Path: "/b", Uninitialised: true},
		{Name: "company-hq", Path: "/a", Readable: false, Reason: "permission denied"},
	})
	if !f.Failing {
		t.Fatal("an unreadable graph must still fail when another is merely empty")
	}
	if !strings.Contains(f.Summary, "company-hq") {
		t.Errorf("the summary must name the broken graph, got %q", f.Summary)
	}
}

// The reason text is the operator's only clue, so "cannot read" and "could not
// find out" must not read the same. The first version reported a manifest that
// `test -r` confirmed was readable as unreadable, because the sudo probe itself
// was refused.
func TestAProbeThatCouldNotRunSaysSoRatherThanBlamingTheFile(t *testing.T) {
	f := EvaluateGraphs([]Graph{
		{Name: "company-hq", Path: "/a", Readable: false,
			Reason: "could not test whether workgraph can read /a/manifest: sudo refused (sudo: a password is required)"},
	})
	if !f.Failing {
		t.Fatal("an undetermined result is still a failure: the question went unanswered")
	}
	if !strings.Contains(f.Summary, "could not test") {
		t.Errorf("the summary should say the probe failed, not that the file is unreadable, got %q",
			f.Summary)
	}
}

// A deployment that does not ingest Workspace is not broken, and must not
// carry a failing check from its first hour. Drive and Meet need a GCP
// project, domain-wide delegation and Pub/Sub, and nothing else depends on
// them, so an install that skips all that is a normal install.
//
// The check still REPORTS, rather than disappearing: a check that vanishes
// when a feature is off leaves nothing to distinguish "deliberately off" from
// "nobody runs this any more".
func TestWorkspaceSourcesOffIsNotAFailure(t *testing.T) {
	at := time.Now()
	th := DefaultThresholds()

	f := EvaluateWorkspaceSources(nil, false, at, th)
	if f.Failing {
		t.Error("ingestion declared off is reported as a failure")
	}
	if f.Summary == "" {
		t.Error("the check went silent instead of saying ingestion is off")
	}
	if f.Observed["ingest"] != false {
		t.Errorf("the finding does not record that ingest is off: %v", f.Observed)
	}

	// Sources left behind after switching ingestion off are worth seeing, so
	// the count is reported even though nothing is failing.
	f = EvaluateWorkspaceSources([]WorkspaceSource{
		{Name: "All", Kind: "drive"},
	}, false, at, th)
	if f.Failing {
		t.Error("ingestion off with a leftover source should not fail")
	}
	if f.Observed["sources"] != 1 {
		t.Errorf("the leftover source is not reported: %v", f.Observed)
	}

	// And with ingestion ON, an empty list is still the alert it was: that is
	// the case where "not set up" and "broken" are indistinguishable, and the
	// deployment has declared which one it is.
	if f := EvaluateWorkspaceSources(nil, true, at, th); !f.Failing {
		t.Error("ingestion declared on with no sources must still fail")
	}
}
