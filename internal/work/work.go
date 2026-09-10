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
	// KindWork runs one bead.
	KindWork Kind = "work"
	// KindFile writes a plan made elsewhere into the graph. It runs no agent
	// and spends nothing: the decision has already been made, and this only
	// records it.
	KindFile Kind = "file"
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
	case KindWork:
		if strings.TrimSpace(j.Bead) == "" {
			return fmt.Errorf("a work job needs a bead")
		}
	case KindFile:
		if strings.TrimSpace(j.Brief) == "" {
			return fmt.Errorf("a file job needs a plan to file")
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
				"Work IN PLACE, at the paths the files belong at: that checkout IS the "+
				"repository, so a portal that should live at portal/ goes at portal/ and "+
				"not in a copy of the repository somewhere else. Do not clone it, do not "+
				"scaffold into a temporary directory meaning to move it in afterwards, "+
				"and do not create a second checkout to work in. Your run can be stopped "+
				"at any moment, and whatever is in the tree at that instant is what gets "+
				"committed -- so a two-step plan that ends in a move leaves the wrong "+
				"thing in the pull request if it is stopped in the middle. It was: one "+
				"run scaffolded a whole portal under .verify-tmp/repo/ and was stopped "+
				"before the move, and the pull request showed that path instead of a "+
				"portal. "+
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
