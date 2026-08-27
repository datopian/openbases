package work

import (
	"strings"
	"testing"
)

func TestAPlanJobNeedsABrief(t *testing.T) {
	if err := (Job{Kind: KindPlan, Cell: "oss"}).Validate(); err == nil {
		t.Fatal("a plan job with no brief was accepted")
	}
}

func TestAWorkJobNeedsABead(t *testing.T) {
	if err := (Job{Kind: KindWork, Cell: "oss"}).Validate(); err == nil {
		t.Fatal("a work job with no bead was accepted")
	}
}

func TestAnUnknownKindIsRefused(t *testing.T) {
	if err := (Job{Kind: "demolish", Cell: "oss"}).Validate(); err == nil {
		t.Fatal("an unknown job kind was accepted")
	}
}

// The planning prompt has to forbid doing the work. An agent handed a brief will
// otherwise start implementing it, and the whole point of a planning step is a
// plan somebody can look at before any of it runs.
func TestThePlanningPromptForbidsDoingTheWork(t *testing.T) {
	got := Job{Kind: KindPlan, Cell: "oss", Brief: "Add a changelog"}.Instructions()
	if !strings.Contains(got, "Add a changelog") {
		t.Error("the brief is not in the instructions")
	}
	for _, must := range []string{"Do NOT do any of the work", "do not modify files", "acceptance"} {
		if !strings.Contains(got, must) {
			t.Errorf("the planning prompt is missing %q", must)
		}
	}
}

// A ceiling on how many. "Break this down" against a broad brief produces forty
// beads, and forty queued agents is a bill rather than a plan.
func TestThePlanningPromptBoundsHowManyBeads(t *testing.T) {
	got := Job{Kind: KindPlan, Cell: "oss", Brief: "b"}.Instructions()
	if !strings.Contains(got, "between 2 and 8") {
		t.Errorf("no ceiling on the number of beads: %s", got)
	}
}

func TestTheWorkPromptDoesNotInviteAFalseClose(t *testing.T) {
	got := Job{Kind: KindWork, Cell: "oss", Bead: "wg-1"}.Instructions()
	if !strings.Contains(got, "wg-1") {
		t.Error("the instructions do not name the bead")
	}
	if !strings.Contains(got, "not before") {
		t.Error("the instructions should say when NOT to close")
	}
}
