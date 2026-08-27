// Package work is the shape of a dispatched job and the decisions around it.
//
// The control plane cannot reach an execution node — nodes have no inbound port
// and reach the control API outward through Access — so a dispatch is a row the
// node comes and claims rather than a call it receives. This package holds the
// job, the instructions an agent is given for it, and the rules about which
// jobs are runnable, separately from the HTTP and the process management.
package work

import (
	"fmt"
	"strings"
)

// Kind is what a job asks for.
type Kind string

const (
	// KindPlan turns a brief into beads. It writes work and does none.
	KindPlan Kind = "plan"
	// KindWork runs one bead.
	KindWork Kind = "work"
)

// Job is one claimed unit of work.
type Job struct {
	ID    string `json:"id"`
	Kind  Kind   `json:"kind"`
	Cell  string `json:"cell"`
	Rig   string `json:"rig"`
	Bead  string `json:"bead,omitempty"`
	Brief string `json:"brief,omitempty"`
}

// Result is what the node reports back.
type Result struct {
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
}

// Validate refuses a job that cannot be run, before an agent is started.
func (j Job) Validate() error {
	switch j.Kind {
	case KindPlan:
		if strings.TrimSpace(j.Brief) == "" {
			return fmt.Errorf("a plan job needs a brief")
		}
	case KindWork:
		if strings.TrimSpace(j.Bead) == "" {
			return fmt.Errorf("a work job needs a bead")
		}
	default:
		return fmt.Errorf("unknown job kind %q", j.Kind)
	}
	if strings.TrimSpace(j.Cell) == "" {
		return fmt.Errorf("a job needs a cell")
	}
	return nil
}

// Instructions is what the agent is told to do.
//
// Here rather than in internal/runner because what an agent is asked to do is a
// product decision, and the planner should have no opinions about it. Here
// rather than in the API because the node is what runs it, and a prompt that
// travels over the wire is a prompt somebody can change in flight.
func (j Job) Instructions() string {
	switch j.Kind {
	case KindPlan:
		// The planning prompt carries three constraints that are all failures
		// somebody would otherwise have to discover.
		//
		// Beads and nothing else, because an agent given a brief will happily
		// start implementing it, and the point of planning is to produce a plan
		// somebody can look at before any of it runs.
		//
		// A ceiling on how many, because "break this down" against a broad brief
		// produces forty beads, and forty queued agents is a bill rather than a
		// plan.
		//
		// Acceptance criteria on each, because a bead without them cannot be
		// judged done, and the agent that later picks it up has nothing to aim
		// at.
		return fmt.Sprintf(
			"Read this brief and turn it into beads. Do NOT do any of the work itself, "+
				"do not modify files, and do not open pull requests.\n\n"+
				"BRIEF:\n%s\n\n"+
				"Create between 2 and 8 beads with `bd create`. Each one must be a single "+
				"piece of work somebody could pick up on its own, with a title that says what "+
				"it delivers, a description explaining why it is needed, and acceptance "+
				"criteria that make it possible to tell when it is done. Where one bead must "+
				"happen before another, record that with `bd dep add`.\n\n"+
				"When you are finished, list the bead ids you created.",
			strings.TrimSpace(j.Brief))

	case KindWork:
		return fmt.Sprintf(
			"Work the bead %s. Read it first, do what it asks, and close it when the work "+
				"is done and not before. If you cannot complete it, leave it open and say why "+
				"in a comment.", j.Bead)
	}
	return ""
}

// Role decides the model tier the job runs on.
//
// Planning is a polecat too. It was tempting to give it a cheaper tier, since it
// writes no code — but breaking a brief into work that somebody can act on is
// judgement, and it is the step whose mistakes are multiplied by every agent
// that acts on its output.
func (j Job) Role() string { return "polecat" }
