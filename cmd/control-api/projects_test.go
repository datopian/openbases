package main

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
			rig, why, decided := chooseRig(c.found, c.wanted)
			if len(c.found) == 0 {
				if decided {
					t.Fatalf("chooseRig decided %q with no candidates", rig)
				}
				return
			}
			if !decided {
				t.Fatal("chooseRig did not decide")
			}
			if c.refused {
				if why == "" {
					t.Fatalf("expected a refusal, got rig %q", rig)
				}
				if rig != "" {
					t.Errorf("refused and still returned rig %q", rig)
				}
				for _, want := range c.says {
					if !strings.Contains(why, want) {
						t.Errorf("the refusal does not mention %q: %s", want, why)
					}
				}
				return
			}
			if why != "" {
				t.Fatalf("unexpected refusal: %s", why)
			}
			if rig != c.rig {
				t.Errorf("routed to %q, want %q", rig, c.rig)
			}
		})
	}
}
