package work

import (
	"strings"
	"testing"
)

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

// 'plan' is one of the unknown kinds now.
//
// Planning left the platform: a plan job started an agent to decide what the
// work was, and deciding happens outside now. A node that still received one
// -- an old row, a hand-written insert -- must refuse it rather than start an
// agent against a prompt that no longer exists.
func TestAPlanJobIsNoLongerAKindOfJob(t *testing.T) {
	if err := (Job{Kind: "plan", Cell: "oss", Brief: "a brief"}).Validate(); err == nil {
		t.Fatal("a planning job was accepted after planning was removed")
	}
}

// Closing a bead is an assertion about the world, so the prompt has to say
// when NOT to close one. An agent that closes on "I did some of it" produces a
// graph that says the work is finished when it is not.
func TestTheWorkPromptDoesNotInviteAFalseClose(t *testing.T) {
	got := Job{Kind: KindWork, Cell: "oss", Bead: "wg-1"}.Instructions()
	if !strings.Contains(got, "wg-1") {
		t.Error("the instructions do not name the bead")
	}
	if !strings.Contains(got, "not before") {
		t.Error("the instructions should say when NOT to close")
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
		// Matched lowercased, and on the shortest phrase that carries the
		// meaning. The previous version wanted the exact string "Do NOT try
		// to commit", which failed when the same instruction was reworded to
		// "do not commit, branch, push, or open a pull request yourself" --
		// a test guarding the capitalisation of a sentence rather than what
		// the agent is told.
		got := strings.ToLower(j.Instructions())
		for _, want := range []string{
			"do not commit",  // landing is not the agent's to do
			"automatically",  // ...because it happens for it, after the run
			"close the bead", // the run ends by closing the bead
			"you have a shell",
			// And it is told to USE the shell to check its work. Without
			// this, granting the shell on 2026-09-09 would have changed what
			// the agent could do and not what it was asked to do.
			"run the tests",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("the instructions do not say %q:\n%s", want, got)
			}
		}
	}
}
