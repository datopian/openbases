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

// The project label reaches the planning agent, and the brief cannot supply it.
//
// The project was decided by whoever enqueued the job, checked against their
// own membership. The brief is untrusted text pasted in from a document, so an
// instruction inside it must not read as the label to use -- which is why the
// project instruction comes after the brief rather than being interpolated
// into it.
func TestPlanPromptCarriesTheProject(t *testing.T) {
	j := Job{Kind: KindPlan, Brief: "Stand up a portal", Project: "roseville-poc"}
	p := j.Instructions()

	if !strings.Contains(p, "wg-project-roseville-poc") {
		t.Fatalf("the project label is missing:\n%s", p)
	}
	if strings.Index(p, "Stand up a portal") > strings.Index(p, "wg-project-roseville-poc") {
		t.Fatal("the project instruction precedes the brief, so the brief could redefine it")
	}

	// No project: no instruction at all, rather than an empty label.
	j.Project = ""
	if got := j.Instructions(); strings.Contains(got, "wg-project-") {
		t.Fatalf("a projectless plan mentions a project label:\n%s", got)
	}
}

// A work job names the checkout, so the agent does not have to find it.
//
// sa-4yn is about PortalJS and reported "no PortalJS source code accessible
// anywhere in this environment (checked town/sandbox, .repo.git, mayor/rig,
// refinery/rig, and all polecat sandboxes)". True of the directory it searched,
// false of the machine: town/portaljs held the code. Dispatch now guarantees
// the rig holds the repository the bead's project owns, so the path is known.
func TestAWorkJobNamesTheCheckout(t *testing.T) {
	j := Job{Kind: KindWork, Bead: "sa-4yn", Cell: "oss", Rig: "portaljs",
		Checkout: "/srv/cells/oss/town/portaljs/refinery/rig"}
	got := j.Instructions()
	for _, want := range []string{
		"sa-4yn",
		"/srv/cells/oss/town/portaljs/refinery/rig",
		"rather than looking elsewhere",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the instructions do not mention %q:\n%s", want, got)
		}
	}

	// An older node sends no checkout, and the instruction must still read as
	// a sentence rather than trailing off into a path that is not there.
	bare := Job{Kind: KindWork, Bead: "sa-4yn", Cell: "oss"}.Instructions()
	if strings.Contains(bare, "checked out at") {
		t.Errorf("a job with no checkout still claims one:\n%s", bare)
	}
	// The property this protects is that the sentence does not trail off into
	// a path that is not there -- not that it ends with any particular word,
	// which is what it used to assert and which broke the moment a paragraph
	// was added after it.
	if strings.Contains(bare, "checked out at .") || strings.Contains(bare, "work there") {
		t.Errorf("a job with no checkout still points at one:\n%s", bare)
	}

	// And every work job is told that landing is not its job, with or without
	// a checkout. sa-kfh's second run spent forty model calls discovering that
	// `git` was refused, then left its bead open because the change had not
	// reached the repository -- correct reasoning from an incomplete brief.
	for _, j := range []Job{
		{Kind: KindWork, Bead: "sa-kfh", Cell: "oss", Checkout: "/srv/x"},
		{Kind: KindWork, Bead: "sa-kfh", Cell: "oss"},
	} {
		got := j.Instructions()
		for _, want := range []string{"Do NOT try to commit", "automatically", "close the bead"} {
			if !strings.Contains(got, want) {
				t.Errorf("the instructions do not say %q:\n%s", want, got)
			}
		}
	}
}
