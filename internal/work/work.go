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
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`
	Cell string `json:"cell"`
	Rig  string `json:"rig"`
	// Checkout is where the rig's code is on the node, filled in by the
	// dispatcher because only the node knows its own paths. Empty is allowed
	// and simply omits the line: an older node sends no such thing.
	Checkout string `json:"-"`
	Bead     string `json:"bead,omitempty"`
	Brief    string `json:"brief,omitempty"`
	// Project is the slug the work is filed into, verified against the
	// requester's membership when the job was enqueued. Empty is company-wide
	// work; it is not a default the runner may choose, which is why it travels
	// with the job rather than being read from anywhere on the node.
	Project string `json:"project,omitempty"`
	// Check is the repository's own build or test command, or empty when
	// nobody has opted this repository in. It travels with the job because the
	// control plane knows which repository the rig holds and the node does
	// not.
	Check string `json:"check,omitempty"`
	// CloneURL is where the rig's repository comes from, sent so the node can
	// create the rig if it does not have it yet.
	//
	// Travels with the job for the same reason Check does: the node has no
	// database. Routing sends work to the rig a cell SHOULD hold, not only one
	// it already has, so that attaching a repository and dispatching against it
	// needs no deploy in between -- and the node then needs to know what to
	// clone. Empty when the rig already exists, which is the common case.
	CloneURL string `json:"clone_url,omitempty"`
	// Prefix is the bead id prefix this rig must be created with.
	//
	// Sent with the clone URL because it is the same fact and has the same
	// source: system_rigs_wanted derives both, and a rig created with a prefix
	// gt chose for itself would produce bead ids that disagree with the ones
	// already recorded -- permanently, since a bead id is never rewritten.
	Prefix string `json:"prefix,omitempty"`
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
		// The project instruction is separate from the brief and comes
		// AFTER it, so a brief that says "file these under x" cannot read as
		// the label to use: the project was decided by whoever enqueued the
		// job, against their own membership, and the brief is untrusted text
		// pasted in from a document.
		project := ""
		if p := strings.TrimSpace(j.Project); p != "" {
			project = fmt.Sprintf(
				"Label every bead you create with `wg-project-%s`, and no other project "+
					"label. This work belongs to that project and its label is what "+
					"decides who can read it.\n\n", p)
		}
		return fmt.Sprintf(
			"Read this brief and turn it into beads. Do NOT do any of the work itself, "+
				"do not modify files, and do not open pull requests.\n\n"+
				"BRIEF:\n%s\n\n"+
				"%s"+
				"Create between 2 and 8 beads with `bd create`. Each one must be a single "+
				"piece of work somebody could pick up on its own, with a title that says what "+
				"it delivers, a description explaining why it is needed, and acceptance "+
				"criteria that make it possible to tell when it is done. Where one bead must "+
				"happen before another, record that with `bd dep add`.\n\n"+
				"When you are finished, list the bead ids you created.",
			strings.TrimSpace(j.Brief), project)

	case KindWork:
		// The checkout is NAMED, not left to be found.
		//
		// Without it an agent searches, and what it finds is whatever else is
		// in reach: sa-4yn is about PortalJS and reported "no PortalJS source
		// code accessible anywhere in this environment (checked town/sandbox,
		// .repo.git, mayor/rig, refinery/rig, and all polecat sandboxes)".
		// That was true of the directory it looked in and false of the machine
		// -- town/portaljs held the code. Dispatch now guarantees the rig holds
		// the repository the bead's project owns, so the path is known and
		// saying it costs one line.
		where := ""
		if c := strings.TrimSpace(j.Checkout); c != "" {
			where = fmt.Sprintf(
				" The code this bead is about is checked out at %s; work there, "+
					"and if what the bead describes is not in that checkout, say so "+
					"rather than looking elsewhere on the machine.", c)
		}
		// Landing is NOT the agent's job, and saying so is the difference
		// between a run that finishes and one that spends its budget on a wall.
		//
		// sa-kfh's second run made the rename correctly and then reported:
		//
		//	I couldn't complete the landing step: every `git` and `gt`
		//	invocation in this session (even read-only ones like `git status`)
		//	is immediately rejected with "This command requires approval"
		//
		// which is exactly right -- the tool policy grants Read, Grep, Glob,
		// Edit, Write and `Bash(bd:*)` and nothing else -- and it left the
		// bead open on the reasonable grounds that the work had not reached
		// the repository. Forty model calls and 83 cents, double the run that
		// simply made the change, and a bead the interface then labelled
		// `blocked` when its work was in a pull request.
		//
		// The agent was not wrong about anything. It had not been told that
		// the branch and the pull request happen after the run, without it.
		//
		// The half of that text which said "you have no access to them and
		// asking will not get it" stopped being true on 2026-09-09, when
		// polecat was given a real shell. Leaving it in would have been worse
		// than the original bug: an agent that CAN run the build being told it
		// cannot will not try, and an instruction the environment contradicts
		// teaches it to distrust the rest of them.
		//
		// So the division of labour is stated as a division of labour, which
		// is what it always was -- landing happens after the run, from a
		// snapshot taken before it -- rather than as a wall.
		return fmt.Sprintf(
			"Work the bead %s. Read it first, do what it asks, and close it when the work "+
				"is done and not before. If you cannot complete it, leave it open and say why "+
				"in a comment.%s\n\n"+
				"You have a shell. Use it: run the build, run the tests, run the linter, "+
				"install what the project needs, read the code with git log and git diff. "+
				"Do not report work as done that you could have checked and did not -- if "+
				"there is a way to verify the change on this machine, run it, and say in the "+
				"bead what you ran and what it printed. If you make scratch files or a "+
				"temporary directory while checking, delete them before you finish -- "+
				"whatever is left in the tree is what gets committed.\n\n"+
				"Landing is not your job and not a restriction on you: after your run "+
				"finishes, whatever you changed is committed to a branch named after this "+
				"bead and opened as a pull request, automatically. So leave your work in the "+
				"working tree and do not commit, branch, push, or open a pull request "+
				"yourself -- `git push` and `gh` are refused, and a commit of your own would "+
				"only confuse what gets landed. Close the bead once the change is made and "+
				"checked; you do not need to see it merged, and waiting for that is not "+
				"something you can do.", j.Bead, where)
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
