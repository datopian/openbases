package domain

import (
	"strings"
	"testing"
	"time"
)

// The acceptance criterion is that every derived status explains its inputs and
// freshness. These assert the property directly, because a signal that lost its
// basis still renders and still looks authoritative.
func TestEverySignalCarriesItsBasis(t *testing.T) {
	now := time.Now()
	d := ProjectDetail{Repositories: []RepositoryStatus{{
		FullName:      "datopian/portaljs",
		LastProjected: &now,
		PullRequests: []PullRequestView{
			{Number: 1, State: "open", UpdatedAt: now, ChecksState: "success"},
			{Number: 2, State: "open", UpdatedAt: now, ChecksState: "failure"},
			{Number: 3, State: "merged", UpdatedAt: now},
		},
	}}}

	for _, s := range deriveSignals(d) {
		if strings.TrimSpace(s.Basis) == "" {
			t.Errorf("signal %q states %q with no basis; a reader cannot disagree with it", s.Name, s.Value)
		}
		if strings.TrimSpace(s.Value) == "" {
			t.Errorf("signal %q has no value", s.Name)
		}
	}
}

// "Never observed" must not be reported as if it were good news. This is the
// failure mode the whole design guards against: an integration that silently
// stopped delivering looks identical to a quiet, healthy project.
func TestUnobservedIsNotReportedAsHealthy(t *testing.T) {
	d := ProjectDetail{Repositories: []RepositoryStatus{
		{FullName: "datopian/nged"}, // registered, never projected
	}}

	signals := deriveSignals(d)
	var coverage *Signal
	for i := range signals {
		if signals[i].Name == "Repository coverage" {
			coverage = &signals[i]
		}
	}
	if coverage == nil {
		t.Fatal("no coverage signal at all")
	}
	if !strings.Contains(strings.ToLower(coverage.Value), "not yet observed") {
		t.Errorf("a repository that never reported is described as %q", coverage.Value)
	}
	if !strings.Contains(strings.ToLower(coverage.Basis), "integration is not delivering") {
		t.Error("the basis does not warn that this may be a broken integration rather than a quiet project")
	}
	if coverage.ObservedAt != nil {
		t.Error("an unobserved repository must not carry an observation time")
	}
}

// A project with no repositories says so, rather than reporting zero open pull
// requests as though that were a finding.
func TestNoRepositoriesIsStatedPlainly(t *testing.T) {
	signals := deriveSignals(ProjectDetail{})
	if len(signals) == 0 {
		t.Fatal("an empty project produced no signals at all")
	}
	if !strings.Contains(signals[0].Value, "No repositories") {
		t.Errorf("expected the empty case to be stated, got %q", signals[0].Value)
	}
}

// Freshness comes from the data, not from when the page was rendered. A signal
// stamped with "now" on every load would always look current.
func TestFreshnessComesFromTheData(t *testing.T) {
	old := time.Now().Add(-72 * time.Hour)
	d := ProjectDetail{Repositories: []RepositoryStatus{{
		FullName:      "datopian/giftless",
		LastProjected: &old,
		PullRequests:  []PullRequestView{{Number: 9, State: "open", UpdatedAt: old}},
	}}}

	for _, s := range deriveSignals(d) {
		if s.ObservedAt == nil {
			continue
		}
		if time.Since(*s.ObservedAt) < time.Hour {
			t.Errorf("signal %q reports a fresh observation time for three-day-old data", s.Name)
		}
	}
}

func TestFailingChecksOnlyCountOpenWork(t *testing.T) {
	now := time.Now()
	d := ProjectDetail{Repositories: []RepositoryStatus{{
		FullName:      "r",
		LastProjected: &now,
		PullRequests: []PullRequestView{
			// A closed pull request that failed is history, not a problem to act
			// on, and counting it would make the number never fall.
			{Number: 1, State: "closed", ChecksState: "failure", UpdatedAt: now},
		},
	}}}
	for _, s := range deriveSignals(d) {
		if s.Name == "Failing checks" {
			t.Errorf("a closed pull request was reported as failing: %q", s.Value)
		}
	}
}
