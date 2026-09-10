package work

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Plan is a decision already made, on its way to the graph.
//
// Planning is moving out of the platform. A person plans in Cowork or Codex,
// and files the result here: this is the shape that filing takes. It is
// deliberately the whole plan in one submission rather than a bead at a time,
// because a plan is a graph -- twelve beads with dependencies between them --
// and filing it one call at a time can half-fail and leave that graph broken,
// with no way to tell which half landed.
//
// It is also an UPSERT. The person will re-plan, often, and re-planning must
// revise the beads it already filed rather than duplicate them. The key is the
// planner's own name for each item (Ref), which is what they know; they should
// not have to track that the platform called it sa-7dc.
type Plan struct {
	// Project the beads belong to, by slug. Every bead is labelled with it, so
	// they appear on that project's page rather than loose in a graph.
	Project string     `json:"project"`
	Beads   []PlanBead `json:"beads"`
}

// PlanBead is one item of a plan.
//
// Ref is the planner's name for it and is the only field that must survive
// between re-plans; everything else may change and the bead will be revised in
// place.
type PlanBead struct {
	Ref         string `json:"ref"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Acceptance is what finishing means. Carried because an agent given a
	// title and no criteria invents them and then closes the bead against its
	// own invention.
	Acceptance string `json:"acceptance,omitempty"`
	Design     string `json:"design,omitempty"`
	Type       string `json:"type,omitempty"`
	// Priority as bd numbers it: 0 highest, 4 lowest. Zero means unset rather
	// than P0, because a plan that omits it is not asking for everything to be
	// urgent.
	Priority int      `json:"priority,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	// DependsOn names other beads in THIS plan, by their Ref, or existing
	// beads by their id. Both, because a re-plan adds work that hangs off
	// beads filed months ago.
	DependsOn []string `json:"depends_on,omitempty"`
	// Status lets a filing close or reopen a bead it owns. Left empty on a
	// create, a bead is open.
	Status string `json:"status,omitempty"`
}

// ExternalKey is how a filed bead is recognised on the next re-plan.
//
// Namespaced by project so that two projects may both call something
// "scaffold", and prefixed so that a human reading the bead can see where it
// came from rather than wondering what "scaffold" is doing in external_ref.
func (p PlanBead) ExternalKey(project string) string {
	return "plan:" + project + "/" + p.Ref
}

// Validate refuses a plan that cannot be filed, before anything is written.
//
// All of it, and reported together. A filing that wrote the first six beads
// and then refused the seventh would leave a half-graph that the next re-plan
// has to reconcile, and the person would have to fix their plan one error per
// round trip.
func (p Plan) Validate() error {
	if strings.TrimSpace(p.Project) == "" {
		return fmt.Errorf("a plan needs a project")
	}
	if len(p.Beads) == 0 {
		return fmt.Errorf("a plan needs at least one bead")
	}

	var problems []string
	seen := map[string]bool{}
	refs := map[string]bool{}
	for _, b := range p.Beads {
		refs[strings.TrimSpace(b.Ref)] = true
	}
	for i, b := range p.Beads {
		ref := strings.TrimSpace(b.Ref)
		switch {
		case ref == "":
			problems = append(problems, fmt.Sprintf(
				"bead %d has no ref; the ref is how a re-plan finds this bead again", i+1))
		case seen[ref]:
			problems = append(problems, fmt.Sprintf(
				"ref %q appears twice; refs are the upsert key, so a duplicate would "+
					"make one bead out of two", ref))
		}
		seen[ref] = true

		if strings.TrimSpace(b.Title) == "" {
			problems = append(problems, fmt.Sprintf("%q has no title", ref))
		}
		if b.Priority < 0 || b.Priority > 4 {
			problems = append(problems, fmt.Sprintf(
				"%q has priority %d; bd numbers them 0 (highest) to 4", ref, b.Priority))
		}
		for _, dep := range b.DependsOn {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				problems = append(problems, fmt.Sprintf("%q depends on an empty ref", ref))
				continue
			}
			if dep == ref {
				problems = append(problems, fmt.Sprintf("%q depends on itself", ref))
			}
			// A dependency naming neither a ref in this plan nor something
			// that looks like a bead id is a typo, and typos in a dependency
			// graph are invisible once filed: the bead simply never unblocks.
			if !refs[dep] && !looksLikeBeadID(dep) {
				problems = append(problems, fmt.Sprintf(
					"%q depends on %q, which is not a ref in this plan and does not "+
						"look like a bead id", ref, dep))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("this plan cannot be filed:\n  - %s",
			strings.Join(problems, "\n  - "))
	}
	return nil
}

// looksLikeBeadID is the shape bd gives an id: a prefix, a dash, a suffix.
func looksLikeBeadID(s string) bool {
	prefix, rest, found := strings.Cut(s, "-")
	return found && prefix != "" && rest != "" && !strings.Contains(rest, " ")
}

// DecodePlan reads a plan from the brief a file job carries.
func DecodePlan(brief string) (Plan, error) {
	var p Plan
	if err := json.Unmarshal([]byte(brief), &p); err != nil {
		return Plan{}, fmt.Errorf("this file job's brief is not a plan: %w", err)
	}
	return p, nil
}

// Filed is what a filing reports back: the planner's refs, mapped to the beads
// they became.
//
// Returned rather than left to be looked up, because the whole point is that
// the planner does not have to know the platform's ids -- but the person
// reading the answer does want to see what was created, and a dispatch needs
// a real id.
type Filed struct {
	Ref     string `json:"ref"`
	Bead    string `json:"bead"`
	Created bool   `json:"created"`
}
