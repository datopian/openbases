package work

import (
	"strings"
	"testing"
)

// A plan is refused whole, with every problem named at once.
//
// Filing is a graph, and a filing that wrote six beads and refused the seventh
// would leave a half-graph for the next re-plan to reconcile. Reporting one
// error per round trip would make fixing a twelve-bead plan a twelve-step
// conversation.
func TestAPlanIsRefusedWholeAndSaysEverythingWrong(t *testing.T) {
	err := Plan{
		Project: "msf",
		Beads: []PlanBead{
			{Ref: "scaffold", Title: "Scaffold"},
			{Ref: "scaffold", Title: "Duplicate ref"},
			{Ref: "", Title: "No ref"},
			{Ref: "deploy", Title: ""},
			{Ref: "seed", Title: "Seed", Priority: 9},
			{Ref: "docs", Title: "Docs", DependsOn: []string{"docs"}},
			{Ref: "qa", Title: "QA", DependsOn: []string{"scafold"}},
		},
	}.Validate()
	if err == nil {
		t.Fatal("a plan full of mistakes was accepted")
	}
	for _, want := range []string{
		"appears twice", // the duplicate ref
		"has no ref",    // the missing ref
		`"deploy" has no title`,
		"bd numbers them 0", // the out-of-range priority
		"depends on itself",
		`"scafold"`, // the typo'd dependency, which would silently never unblock
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %s:\n%v", want, err)
		}
	}

	// A dependency on a bead that already exists is fine: a re-plan adds work
	// hanging off beads filed months ago.
	if err := (Plan{Project: "msf", Beads: []PlanBead{
		{Ref: "next", Title: "Next", DependsOn: []string{"sa-7dc"}},
	}}).Validate(); err != nil {
		t.Errorf("a dependency on an existing bead id was refused: %v", err)
	}

	// And the empties.
	if err := (Plan{Beads: []PlanBead{{Ref: "a", Title: "A"}}}).Validate(); err == nil {
		t.Error("a plan with no project was accepted; its beads would be loose in a graph")
	}
	if err := (Plan{Project: "msf"}).Validate(); err == nil {
		t.Error("a plan with no beads was accepted")
	}
}

// The upsert key is namespaced, so two projects may both call something
// "scaffold" and a human can see where the key came from.
func TestTheExternalKeyIsNamespacedByProject(t *testing.T) {
	b := PlanBead{Ref: "scaffold"}
	if got := b.ExternalKey("msf"); got != "plan:msf/scaffold" {
		t.Errorf("key = %q", got)
	}
	if b.ExternalKey("msf") == b.ExternalKey("jopacc") {
		t.Error("two projects filing the same ref collide on one bead")
	}
}

// A file job carries a plan and runs no agent, so it is refused without one.
func TestAFileJobNeedsAPlan(t *testing.T) {
	if err := (Job{Kind: KindFile, Cell: "oss"}).Validate(); err == nil {
		t.Error("a file job with nothing to file was accepted; it would claim a slot " +
			"and produce nothing")
	}
	if err := (Job{Kind: KindFile, Cell: "oss", Brief: `{"project":"msf"}`}).Validate(); err != nil {
		t.Errorf("a file job with a plan was refused: %v", err)
	}
}
