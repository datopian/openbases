package monitor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEvaluateAPI(t *testing.T) {
	for _, tc := range []struct {
		name    string
		obs     APIObservation
		failing bool
		wants   string
	}{
		{"ready", APIObservation{Status: 200, Latency: 12 * time.Millisecond}, false, "ready in 12ms"},
		// The state the silent password reset actually produced: the process is
		// up and answering, and it cannot reach the database. /health/live would
		// have returned 200 straight through this.
		{"up but not ready", APIObservation{Status: 503}, true, "HTTP 503"},
		{"no answer", APIObservation{Err: errors.New("connection refused")}, true, "did not answer"},
		// A transport error takes precedence over the status, because a status
		// of 0 alongside an error would otherwise report "HTTP 0".
		{"error wins over status", APIObservation{Status: 0, Err: errors.New("timeout")}, true, "did not answer"},
		// Anything that is not 200 is a failure, including a redirect. An Access
		// login redirect reaching this check means the probe never got to the
		// origin, which must not read as healthy.
		{"redirect is not healthy", APIObservation{Status: 302}, true, "HTTP 302"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := EvaluateAPI(tc.obs)
			if f.Failing != tc.failing {
				t.Fatalf("Failing = %v, want %v (%s)", f.Failing, tc.failing, f.Summary)
			}
			if !strings.Contains(f.Summary, tc.wants) {
				t.Fatalf("Summary = %q, want it to contain %q", f.Summary, tc.wants)
			}
		})
	}
}

func TestEvaluateWebhook(t *testing.T) {
	th := DefaultThresholds()
	for _, tc := range []struct {
		name    string
		obs     WebhookObservation
		failing bool
	}{
		{"empty queue", WebhookObservation{}, false},
		// The case that must NOT alert: a burst mid-merge, all of it recent.
		// Alerting on depth would fire here, during correct operation.
		{"deep but fresh", WebhookObservation{Unprocessed: 50, OldestUnprocessed: time.Minute}, false},
		// The case that MUST alert: one delivery, stuck. Small and fatal.
		{"shallow but stuck", WebhookObservation{Unprocessed: 1, OldestUnprocessed: 2 * time.Hour}, true},
		// The case observed on staging while healthy: a queue 20 deep whose
		// oldest entry is 9 minutes old, between two 15-minute reconciliation
		// passes. This must NOT alert; a threshold at the reconciliation period
		// would have fired on it.
		{"between reconciliation passes", WebhookObservation{Unprocessed: 20, OldestUnprocessed: 9 * time.Minute}, false},
		// One full period past the deadline is still not an alert, because a
		// single missed pass is ordinary.
		{"one missed pass", WebhookObservation{Unprocessed: 20, OldestUnprocessed: 20 * time.Minute}, false},
		{"exactly at the limit is not yet failing", WebhookObservation{Unprocessed: 1, OldestUnprocessed: th.WebhookBacklogAge}, false},
		{"just past the limit", WebhookObservation{Unprocessed: 1, OldestUnprocessed: th.WebhookBacklogAge + time.Second}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if f := EvaluateWebhook(tc.obs, th); f.Failing != tc.failing {
				t.Fatalf("Failing = %v, want %v (%s)", f.Failing, tc.failing, f.Summary)
			}
		})
	}
}

func TestEvaluateAgentHealth(t *testing.T) {
	th := DefaultThresholds()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		obs     AgentHealthObservation
		failing bool
	}{
		// No cell deployed: silence is correct. Alerting here is how a staging
		// environment trains people to mute the channel.
		{"no cells expected", AgentHealthObservation{Cells: 0}, false},
		{"recent report", AgentHealthObservation{Cells: 2, LastEventAt: now.Add(-5 * time.Minute)}, false},
		{"silent too long", AgentHealthObservation{Cells: 2, LastEventAt: now.Add(-2 * time.Hour)}, true},
		// The blind spot this class exists for: cells deployed, nothing ever
		// reported. An inbox with no agent alerts in it looks identical to
		// healthy agents.
		{"never reported at all", AgentHealthObservation{Cells: 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if f := EvaluateAgentHealth(tc.obs, now, th); f.Failing != tc.failing {
				t.Fatalf("Failing = %v, want %v (%s)", f.Failing, tc.failing, f.Summary)
			}
		})
	}
}

func TestEvaluateDisk(t *testing.T) {
	th := DefaultThresholds()

	// Being unable to measure must not read as healthy.
	if f := EvaluateDisk(nil, th); !f.Failing {
		t.Fatal("no measurable filesystem reported as healthy")
	}

	// The worst mount decides, and the summary names it — an operator sent to
	// the wrong filesystem finds plenty of space and concludes the alert lied.
	f := EvaluateDisk([]Filesystem{
		{Path: "/", UsedPercent: 40},
		{Path: "/var/lib/postgresql", UsedPercent: 91},
	}, th)
	if !f.Failing {
		t.Fatal("a filesystem at 91% did not fail an 85% threshold")
	}
	if !strings.Contains(f.Summary, "/var/lib/postgresql") {
		t.Fatalf("Summary = %q, want it to name the full filesystem", f.Summary)
	}

	if f := EvaluateDisk([]Filesystem{{Path: "/", UsedPercent: 84.9}}, th); f.Failing {
		t.Fatalf("84.9%% failed an 85%% threshold: %s", f.Summary)
	}
}

func TestEvaluateBackup(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	// Declaring nothing must not pass. A check with no obligations reports
	// success forever, which is worse than no check because it is evidence.
	if f := EvaluateBackup(nil, now); !f.Failing {
		t.Fatal("an empty declaration reported as healthy")
	}

	fresh := []BackupStream{{Name: "beads", MaxAge: day, Last: now.Add(-2 * time.Hour)}}
	if f := EvaluateBackup(fresh, now); f.Failing {
		t.Fatalf("a two-hour-old daily backup failed: %s", f.Summary)
	}

	// A stream that has never run is the failure that matters most, and it is
	// the one a discovery-based check would report as nothing to say.
	f := EvaluateBackup([]BackupStream{
		{Name: "beads", MaxAge: day, Last: now.Add(-time.Hour)},
		{Name: "postgres", MaxAge: day, Missing: true},
	}, now)
	if !f.Failing {
		t.Fatal("a backup stream that has never completed reported as healthy")
	}
	if !strings.Contains(f.Summary, "postgres") || strings.Contains(f.Summary, "beads has") {
		t.Fatalf("Summary = %q, want it to blame postgres and not beads", f.Summary)
	}

	// Every stream is judged, not just the first failing one, so one broken
	// stream does not hide a second.
	f = EvaluateBackup([]BackupStream{
		{Name: "beads", MaxAge: day, Last: now.Add(-3 * day)},
		{Name: "postgres", MaxAge: day, Missing: true},
	}, now)
	if !strings.Contains(f.Summary, "beads") || !strings.Contains(f.Summary, "postgres") {
		t.Fatalf("Summary = %q, want both failing streams named", f.Summary)
	}
}

// Every class must have a distinct rule and dedupe key. Two classes sharing
// either would silently collapse into one inbox item, so the second problem
// would be invisible whenever the first was already open.
func TestClassesAreDistinct(t *testing.T) {
	rules, keys := map[string]string{}, map[string]string{}
	for _, c := range Classes {
		if prev, dup := rules[c.Rule]; dup {
			t.Errorf("classes %s and %s share the rule %q", prev, c.Name, c.Rule)
		}
		rules[c.Rule] = c.Name
		if prev, dup := keys[c.DedupeKey()]; dup {
			t.Errorf("classes %s and %s share the dedupe key %q", prev, c.Name, c.DedupeKey())
		}
		keys[c.DedupeKey()] = c.Name
		if c.Score <= 0 || c.Score > 1 {
			t.Errorf("class %s scores %v, outside the 0..1 scale the other inbox rules use", c.Name, c.Score)
		}
	}
	// The five WP-I1 required, plus service (wg-95y), cost_import (wg-7jz) and
	// workspace_sources (wg-8yv.20) — the three that cover failures found by
	// running the platform rather than planning it. Asserted as a count so that
	// adding one is a deliberate edit here rather than something that slips in.
	if len(Classes) != 8 {
		t.Errorf("got %d classes, want 8", len(Classes))
	}
	for _, required := range []string{"api", "webhook", "agent_stall", "disk", "backup",
		"service", "cost_import", "workspace_sources"} {
		found := false
		for _, c := range Classes {
			if c.Name == required {
				found = true
			}
		}
		if !found {
			t.Errorf("class %q is missing", required)
		}
	}
}

// The acceptance criterion is that the alert reaches the operator WITH a runbook
// link. A link to a file that does not exist satisfies the code and fails the
// operator, so the file has to be there.
func TestEveryClassHasARunbookThatExists(t *testing.T) {
	root := repoRoot(t)
	for _, c := range Classes {
		if c.Runbook == "" {
			t.Errorf("class %s has no runbook", c.Name)
			continue
		}
		path := filepath.Join(root, c.Runbook)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("class %s points at %s, which does not exist", c.Name, c.Runbook)
			continue
		}
		// A stub file would pass an existence check while helping nobody.
		if info.Size() < 400 {
			t.Errorf("class %s points at %s, which is only %d bytes and cannot be a usable procedure",
				c.Name, c.Runbook, info.Size())
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the repository root")
		}
		dir = parent
	}
}

// ---------------------------------------------------------------------------
// Service liveness (wg-95y)
// ---------------------------------------------------------------------------

// The failure this class exists for: the worker dies, reconciliation keeps
// draining the backlog, every downstream signal stays green, and freshness
// silently degrades. Only asking about the unit catches it.
func TestADeadWorkerIsReportedEvenWhenNothingElseLooksWrong(t *testing.T) {
	f := EvaluateServices([]ServiceState{
		{Name: "control-api.service", Active: true, Detail: "active"},
		{Name: "workgraph-worker.service", Active: false, Detail: "failed"},
		{Name: "wg-monitor.timer", Active: true, Detail: "active"},
	})
	if !f.Failing {
		t.Fatal("a dead worker was reported as healthy")
	}
	if !strings.Contains(f.Summary, "workgraph-worker.service") {
		t.Errorf("the summary must name the unit, or an operator looks at the wrong one: %s", f.Summary)
	}
	if !strings.Contains(f.Summary, "failed") {
		t.Errorf("systemd's own word distinguishes crashed from never started: %s", f.Summary)
	}
}

func TestAllServicesRunningPasses(t *testing.T) {
	f := EvaluateServices([]ServiceState{
		{Name: "control-api.service", Active: true, Detail: "active"},
		{Name: "workgraph-worker.service", Active: true, Detail: "active"},
	})
	if f.Failing {
		t.Fatalf("healthy services reported as failing: %s", f.Summary)
	}
}

// Consistent with the backup check: nothing declared means nothing is watched,
// and reporting that as healthy is how a broken check becomes invisible.
func TestNoDeclaredServiceIsAFailure(t *testing.T) {
	if f := EvaluateServices(nil); !f.Failing {
		t.Fatal("an empty service list passed, so nothing is being watched and nothing says so")
	}
}

// A unit systemd does not know about is not running. "unknown" is reported
// rather than an empty string, because a blank in an inbox item is not an answer.
func TestAnAbsentUnitIsDownAndSaysSo(t *testing.T) {
	f := EvaluateServices([]ServiceState{{Name: "wg-ghost.service", Active: false}})
	if !f.Failing {
		t.Fatal("a unit that does not exist was treated as running")
	}
	if !strings.Contains(f.Summary, "unknown") {
		t.Errorf("expected the state to read as unknown: %s", f.Summary)
	}
}

// ---------------------------------------------------------------------------
// Cost import freshness (wg-7jz)
// ---------------------------------------------------------------------------

func importThresholds() Thresholds {
	t := DefaultThresholds()
	t.CostImportMaxAge = 3 * time.Hour
	return t
}

// The distinction this check turns on: a QUIET gateway is not a STOPPED
// importer. Measuring the newest usage record instead alerts loudest on the
// most idle environment, which is what made the budget check refuse every
// dispatch on staging while the importer ran perfectly.
func TestAQuietGatewayIsNotAStoppedImporter(t *testing.T) {
	now := time.Now()
	f := EvaluateCostImport([]GatewayImport{
		{Name: "workgraph-staging-oss", Last: now.Add(-10 * time.Minute), Complete: true},
		// Imported minutes ago; its newest usage record is a week old because
		// nobody has used it. That must not be an alert.
		{Name: "workgraph-staging-client", Last: now.Add(-12 * time.Minute), Complete: true},
	}, now, importThresholds())
	if f.Failing {
		t.Fatalf("a quiet but freshly imported gateway was reported as stale: %s", f.Summary)
	}
}

func TestAStoppedImporterIsReported(t *testing.T) {
	now := time.Now()
	f := EvaluateCostImport([]GatewayImport{
		{Name: "workgraph-staging-oss", Last: now.Add(-9 * time.Hour), Complete: true},
	}, now, importThresholds())
	if !f.Failing {
		t.Fatal("a nine-hour-old import passed a three-hour threshold")
	}
	if !strings.Contains(f.Summary, "workgraph-staging-oss") {
		t.Errorf("the summary must name the gateway: %s", f.Summary)
	}
}

func TestAGatewayNeverImportedIsReported(t *testing.T) {
	now := time.Now()
	f := EvaluateCostImport([]GatewayImport{
		{Name: "workgraph-staging-internal"},
	}, now, importThresholds())
	if !f.Failing {
		t.Fatal("a gateway that has never been imported passed")
	}
	if !strings.Contains(f.Summary, "never") {
		t.Errorf("never-imported and stale are different states: %s", f.Summary)
	}
}

// Running but not caught up. Fresh by age and still wrong to trust, because a
// budget decision resting on a partial read is resting on the wrong number.
func TestAnIncompleteImportIsReportedEvenThoughItIsRecent(t *testing.T) {
	now := time.Now()
	f := EvaluateCostImport([]GatewayImport{
		{Name: "workgraph-staging-oss", Last: now.Add(-2 * time.Minute), Complete: false},
	}, now, importThresholds())
	if !f.Failing {
		t.Fatal("an import that had not caught up was reported as healthy")
	}
	if !strings.Contains(f.Summary, "caught up") {
		t.Errorf("the summary should distinguish incomplete from stale: %s", f.Summary)
	}
}

func TestNoImportRecordedAtAllIsAFailure(t *testing.T) {
	if f := EvaluateCostImport(nil, time.Now(), importThresholds()); !f.Failing {
		t.Fatal("no import record at all was treated as healthy")
	}
}
