package publish

import (
	"strings"
	"testing"
)

func TestTitleKeepsTheFirstSentence(t *testing.T) {
	got := title("Send CDT the revised scope by Friday. Monika will draft it and Osahon reviews.")
	want := "Send CDT the revised scope by Friday"
	if got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
}

func TestTitleTrimsAtAWordBoundary(t *testing.T) {
	long := strings.Repeat("scope ", 60)
	got := title(long)
	if len(got) > titleLimit+3 {
		t.Fatalf("title is %d characters: %q", len(got), got)
	}
	if strings.HasSuffix(got, "sco...") {
		t.Fatalf("title cut a word in half: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("a truncated title should say so: %q", got)
	}
}

func TestTitleCollapsesWhitespace(t *testing.T) {
	if got := title("  Two\n\nlines  become one  "); got != "Two lines become one" {
		t.Fatalf("title = %q", got)
	}
}

// A statement is a decision about words, so the full text has to survive
// somewhere even when the title is a summary of it.
func TestTheStatementSurvivesInFull(t *testing.T) {
	statement := "Send CDT the revised scope by Friday. " + strings.Repeat("Detail. ", 40)
	c := Pending{CandidateID: "c1", Type: "commitment", Statement: statement,
		SourceID: "s1", Visibility: "restricted", ProjectSlug: "cdt"}
	issue := c.issue(LabelPrefix + "c1")

	if !strings.HasPrefix(issue.Description, statement) {
		t.Fatalf("the description does not open with the statement: %q", issue.Description)
	}
	if issue.Title == statement {
		t.Fatalf("the title should be a summary, not the whole statement")
	}
	if !strings.Contains(issue.Description, "Classification: restricted") {
		t.Fatalf("the classification has to travel with the work: %q", issue.Description)
	}
}

// The label is the idempotency key. A bead created without it would be
// published a second time on the next pass.
func TestTheCandidateLabelIsAlwaysSet(t *testing.T) {
	c := Pending{CandidateID: "c1", Type: "risk", Statement: "x", ProjectSlug: "nged"}
	issue := c.issue(LabelPrefix + "c1")

	var found, kind, project bool
	for _, l := range issue.Labels {
		switch l {
		case LabelPrefix + "c1":
			found = true
		case "wg-risk":
			kind = true
		case "wg-project-nged":
			project = true
		}
	}
	if !found {
		t.Fatalf("no candidate label in %v", issue.Labels)
	}
	// bd has no "risk" type, so the real type has to be recoverable from the
	// bead rather than lost in the mapping to chore.
	if !kind {
		t.Fatalf("the candidate type is not recorded as a label: %v", issue.Labels)
	}
	if !project {
		t.Fatalf("the project is not recorded as a label: %v", issue.Labels)
	}
	if issue.ExternalRef != "wg-candidate-c1" {
		t.Fatalf("external ref = %q", issue.ExternalRef)
	}
}

func TestTypesMapOntoTypesBdAccepts(t *testing.T) {
	accepted := map[string]bool{"bug": true, "feature": true, "task": true,
		"epic": true, "chore": true, "decision": true}
	for _, candidateType := range []string{"task", "commitment", "risk", "question", "market-signal"} {
		if got := beadType(candidateType); !accepted[got] {
			t.Fatalf("%s maps to %q, which bd would refuse", candidateType, got)
		}
	}
}

// Nobody has agreed to do a risk or a question, so neither may become a task:
// a graph where every accepted statement is a task cannot be triaged.
func TestOnlyAgreedWorkBecomesATask(t *testing.T) {
	for _, candidateType := range []string{"risk", "question", "market-signal"} {
		if beadType(candidateType) == "task" {
			t.Fatalf("%s became a task", candidateType)
		}
	}
	for _, candidateType := range []string{"task", "commitment"} {
		if beadType(candidateType) != "task" {
			t.Fatalf("%s did not become a task", candidateType)
		}
	}
}

func TestPriorityOrdersRiskAndCommitmentAboveTasks(t *testing.T) {
	if priority("risk") >= priority("task") {
		t.Fatalf("a risk does not outrank a task")
	}
	if priority("commitment") >= priority("task") {
		t.Fatalf("a commitment does not outrank a task")
	}
	if priority("question") <= priority("task") {
		t.Fatalf("a question outranks a task")
	}
}

func TestAPublisherWithoutItsDependenciesRefuses(t *testing.T) {
	p := &Publisher{}
	if _, err := p.Run(t.Context(), 10); err == nil {
		t.Fatal("a publisher with no database and no client ran anyway")
	}
}
