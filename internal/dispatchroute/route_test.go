package dispatchroute

import (
	"strings"
	"testing"
)

// Naming a rig chooses between the rigs that can do the work. It is not a way
// past the question.
//
// The handler's first version skipped the routing check entirely whenever the
// caller named a rig, so `{"rig":"sandbox"}` on a PortalJS bead reproduced
// exactly the failure the check was written to prevent: an agent running in a
// disposable sandbox, finding no PortalJS source, and reporting done after
// about 78 cents. The check has to apply to the named rig too.
func TestANamedRigMustBeOneThatHoldsTheWork(t *testing.T) {
	portaljs := candidate{rig: "portaljs", repo: "datopian/portaljs"}
	cloud := candidate{rig: "cloud_portaljs_com", repo: "datopian/cloud.portaljs.com"}

	for _, c := range []struct {
		name    string
		found   []candidate
		wanted  string
		rig     string
		refused bool
		says    []string
	}{{
		name:  "the only candidate needs no naming",
		found: []candidate{portaljs},
		rig:   "portaljs",
	}, {
		name:   "a named candidate is honoured",
		found:  []candidate{portaljs, cloud},
		wanted: "cloud_portaljs_com",
		rig:    "cloud_portaljs_com",
	}, {
		// The whole point. sandbox is a real rig on the real cell, and it is
		// where the 78 cents went.
		name:    "a rig that does not hold the work is refused however explicitly it is named",
		found:   []candidate{portaljs, cloud},
		wanted:  "sandbox",
		refused: true,
		says:    []string{"sandbox", "portaljs", "cloud_portaljs_com"},
	}, {
		// Even when it is the ONLY candidate: a single match does not make an
		// unrelated name acceptable.
		name:    "naming an unrelated rig is refused when there is exactly one candidate",
		found:   []candidate{portaljs},
		wanted:  "sandbox",
		refused: true,
		says:    []string{"sandbox", "portaljs"},
	}, {
		name:    "two candidates and no name asks which",
		found:   []candidate{portaljs, cloud},
		refused: true,
		says:    []string{"name one", "portaljs", "cloud_portaljs_com"},
	}, {
		// Nothing matched: this function has no answer, and the caller has to
		// go back to the database to say why.
		name:  "no candidates is not a decision",
		found: nil,
	}} {
		t.Run(c.name, func(t *testing.T) {
			rig, refused, decided := Choose(c.found, c.wanted)
			if len(c.found) == 0 {
				if decided {
					t.Fatalf("Choose decided %q with no candidates", rig)
				}
				return
			}
			if !decided {
				t.Fatal("Choose did not decide")
			}
			if c.refused {
				if refused == nil {
					t.Fatalf("expected a refusal, got rig %q", rig)
				}
				if rig != "" {
					t.Errorf("refused and still returned rig %q", rig)
				}
				// Every refusal from Choose is a rig problem: a bead that
				// does not exist is refused earlier, by For, before there are
				// any candidates to choose between.
				if refused.Code != CodeNoRig {
					t.Errorf("code is %q, want %q", refused.Code, CodeNoRig)
				}
				for _, want := range c.says {
					if !strings.Contains(refused.Why, want) {
						t.Errorf("the refusal does not mention %q: %s", want, refused.Why)
					}
				}
				return
			}
			if refused != nil {
				t.Fatalf("unexpected refusal: %s", refused.Why)
			}
			if rig != c.rig {
				t.Errorf("routed to %q, want %q", rig, c.rig)
			}
		})
	}
}

// A refusal carries a code a client can branch on, and the two kinds are
// different codes because they need different actions.
//
// Both were no_rig_for_project, which was wrong the moment a bead that does
// not exist could be refused: a client reading that code would send somebody
// to provision a rig for a typo, and the prose carried the real meaning while
// the machine-readable field did not.
func TestARefusalSaysWhichKindItIs(t *testing.T) {
	// Choose only ever refuses for rig reasons: a bead that does not exist is
	// refused by For, before there are candidates to choose between.
	_, refused, decided := Choose(
		[]candidate{{rig: "a", repo: "o/a"}, {rig: "b", repo: "o/b"}}, "")
	if !decided || refused == nil {
		t.Fatal("two candidates and no name should be refused")
	}
	if refused.Code != CodeNoRig {
		t.Errorf("ambiguity is coded %q, want %q", refused.Code, CodeNoRig)
	}

	// The codes are distinct strings, which is the whole point.
	if CodeNoRig == CodeNotProjected {
		t.Error("the two codes are the same string, so a client cannot tell them apart")
	}
	// And they are stable identifiers rather than prose: a client matches on
	// them, so they must not read like sentences.
	for _, code := range []string{CodeNoRig, CodeNotProjected} {
		if strings.ContainsAny(code, " .") || code != strings.ToLower(code) {
			t.Errorf("%q is not a stable machine-readable code", code)
		}
	}

	// A refusal is usable as an error, so a caller that only wants to bubble it
	// up does not have to reach inside.
	var err error = &Refusal{Code: CodeNotProjected, Why: "no bead x has been projected"}
	if !strings.Contains(err.Error(), "projected") {
		t.Errorf("a refusal does not read as an error: %v", err)
	}
}

// A plan for a project no rig in the cell holds is refused, not filed.
//
// Filing it somewhere arbitrary is worse than refusing: the beads exist, look
// filed, and no dispatch will ever find them. This is the same failure that
// put two msf beads in the sandbox graph.
func TestAProjectWithNoRigIsRefusedRatherThanFiledAnywhere(t *testing.T) {
	rig, refusal, decided := Choose(nil, "")
	if decided {
		t.Fatalf("nothing held the project and Choose decided anyway: %q %v", rig, refusal)
	}
}

// Naming a rig that does not hold the project is refused, for filing as for
// dispatch: naming one is a choice among the rigs that can do the work, not a
// way past the check.
func TestNamingARigThatDoesNotHoldTheProjectIsRefused(t *testing.T) {
	found := []candidate{{rig: "msf", repo: "datopian/msf"}}
	rig, refusal, decided := Choose(found, "sandbox")
	if !decided || refusal == nil {
		t.Fatalf("naming an unrelated rig was allowed: %q", rig)
	}
	if refusal.Code != CodeNoRig {
		t.Errorf("wrong refusal code: %s", refusal.Code)
	}
}
