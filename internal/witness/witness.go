// Package witness decides what to do about a polecat's health without asking a
// model.
//
// Gas Town ships a Witness: a per-rig agent that detects stalled polecats,
// nudges unresponsive sessions, cleans up zombies, and nukes sandboxes when
// work completes. Every one of those jobs is mechanical, and Gas Town already
// computes the answers in Go — `gt polecat list <rig> --json` returns a
// per-polecat `verdict` of SAFE_TO_NUKE, NEEDS_MQ_SUBMIT or NEEDS_RECOVERY, and
// the agents write liveness to .runtime/heartbeats/<prefix>-<name>.json.
//
// The stock Witness is nevertheless a Claude Code session driven by a long
// prose formula, re-sent on a 300-second patrol cycle for as long as the town is
// up. On one day of testing the patrol roles made 1,616 requests and 365,671
// output tokens — more than the work they were watching. Paying a model to read
// a boolean is the cost problem in miniature.
//
// So this package is the same decision, as a function. Plan section 4.7 orders
// the cost work "zero unnecessary calls" before "cheaper calls", and this is
// what that means concretely: the routine path reaches no model at all. Only a
// genuinely ambiguous case — a session that is alive but has gone quiet, where
// "stuck" and "thinking hard" look identical from outside — is handed onward,
// and by default it is handed to a person via the attention inbox rather than
// to another model.
//
// What this package deliberately does NOT do is reimplement Gas Town. Every
// action it takes is a Gas Town command; every fact it reads is a Gas Town
// output. It replaces a prompt with a program, not an orchestrator with another
// orchestrator (ADR-0005).
package witness

import (
	"fmt"
	"strings"
	"time"
)

// Polecat is one entry of `gt polecat list <rig> --json`.
//
// The fields are Gas Town's, spelled as Gas Town spells them. Only the ones the
// decision actually uses are named; the rest of the object is ignored rather
// than mirrored, so an upstream addition does not break parsing.
type Polecat struct {
	Rig            string `json:"rig"`
	Name           string `json:"name"`
	State          string `json:"state"`
	CleanupStatus  string `json:"cleanup_status"`
	ActiveMR       string `json:"active_mr"`
	Branch         string `json:"branch"`
	Verdict        string `json:"verdict"`
	Reason         string `json:"reason"`
	Reusable       bool   `json:"reusable"`
	SafeToNuke     bool   `json:"safe_to_nuke"`
	NeedsRecovery  bool   `json:"needs_recovery"`
	NeedsMQSubmit  bool   `json:"needs_mq_submit"`
	SessionRunning bool   `json:"session_running"`
}

// Ref is the rig-qualified name Gas Town commands expect: "sandbox/chrome".
func (p Polecat) Ref() string { return p.Rig + "/" + p.Name }

// Heartbeat is .runtime/heartbeats/<prefix>-<name>.json, written by the agent
// itself. It is the only first-hand evidence that a session is still doing
// something; the process being alive proves only that it has not crashed.
type Heartbeat struct {
	Timestamp time.Time `json:"timestamp"`
	State     string    `json:"state"`
	Context   string    `json:"context,omitempty"`
	Bead      string    `json:"bead,omitempty"`
}

// Action is what the witness has decided to do.
type Action string

const (
	// Observe records the state and does nothing. The overwhelming majority of
	// decisions are this one, which is the point.
	Observe Action = "observe"
	// Nuke destroys a finished polecat's session, worktree and branch, freeing
	// capacity. Taken only when Gas Town has already proved nothing is at risk.
	Nuke Action = "nuke"
	// Escalate raises the case for a person. Used where work exists that could
	// be lost, which is never a decision to automate.
	Escalate Action = "escalate"
	// Ambiguous is the one state a program cannot settle: a live session that
	// has gone quiet. Whether that is a stall or a long thought is not visible
	// from the outside, so it is routed onward rather than guessed.
	Ambiguous Action = "ambiguous"
)

// Decision is an action plus the evidence for it.
//
// Basis carries the field values that produced the outcome, in the same spirit
// as the chief-of-staff answers: a health action that cannot be explained
// cannot be trusted, and an operator woken by an escalation needs to see what
// the machine saw, not just its conclusion.
type Decision struct {
	Rig     string    `json:"rig"`
	Polecat string    `json:"polecat"`
	Action  Action    `json:"action"`
	Reason  string    `json:"reason"`
	Basis   string    `json:"basis"`
	Bead    string    `json:"bead,omitempty"`
	Silence string    `json:"silence,omitempty"`
	At      time.Time `json:"at"`
}

// Situation collapses Gas Town's reason into the small set of situations a
// person can actually do something about.
//
// This exists because the reason is NOT stable for a fixed situation, which the
// original keying assumed. Observed on the sandbox rig: the same polecat, needing
// the same action, alternated between reasons depending on whether the town was
// up —
//
//	git-dirty / git-unpushed / cleanup-has_uncommitted   when it was down
//	not-idle                                             when it was up
//
// so each polecat produced two inbox rows describing one problem. Dedup was
// working perfectly; it was being given two different keys for one situation.
//
// The distinction that matters is kept: work that exists and is not safely
// stored is a different job from work that was finished and never submitted,
// which is different again from a session that has gone quiet. The distinction
// that does not matter — which of several git-shaped reasons Gas Town happened
// to report this pass — is dropped. The raw reason still travels in the
// explanation, which is refreshed on every observation, so nothing is lost for
// somebody reading the item.
func (d Decision) Situation() string {
	switch {
	case d.Action == Ambiguous:
		// However the silence was described, the job is the same: look at a
		// session that has stopped talking.
		return "stalled"
	case d.Reason == "never-submitted":
		return "never-submitted"
	case isUnsafelyStoredWork(d.Reason):
		return "work-at-risk"
	default:
		// An unrecognised reason keeps its own identity rather than being folded
		// into work-at-risk. Folding it would be the original bug in reverse:
		// quietly merging two genuinely different problems into one row. A new
		// reason from Gas Town should be noisy enough to notice and classify.
		return "other:" + d.Reason
	}
}

// isUnsafelyStoredWork reports whether the reason describes work that exists on
// a machine and is not safely stored anywhere else.
//
// Matched by family rather than by an exact list, because these strings come
// from Gas Town and a new git- or cleanup- variant is a rename of something
// already covered rather than a new situation.
func isUnsafelyStoredWork(reason string) bool {
	switch reason {
	case "not-idle", "needs-recovery":
		return true
	}
	return strings.HasPrefix(reason, "git-") || strings.HasPrefix(reason, "cleanup-")
}

// DedupeKey identifies the situation, not the observation.
//
// A stalled polecat is still stalled on the next pass, and an inbox that grows
// one identical row per tick is worse than no inbox. Keyed on the SITUATION
// rather than the raw reason, so the same problem collapses to one item even
// when Gas Town describes it differently between passes.
func (d Decision) DedupeKey() string {
	return fmt.Sprintf("witness:%s/%s:%s", d.Rig, d.Polecat, d.Situation())
}

// StallAfter is how long a live session may stay silent before the case is
// called ambiguous.
//
// Twelve minutes, because a polecat running a test suite or a long build is
// routinely quiet for several minutes and calling that a stall produces exactly
// the false nudges the stock Witness is criticised for. The cost of waiting is
// a few idle minutes; the cost of interrupting is a lost train of thought and a
// re-primed context.
const StallAfter = 12 * time.Minute

// Decide returns what to do about one polecat.
//
// hb may be nil, which means the agent has not written a heartbeat yet. That is
// normal for the first seconds of a session and is not evidence of a stall.
func Decide(p Polecat, hb *Heartbeat, now time.Time, stallAfter time.Duration) Decision {
	d := Decision{Rig: p.Rig, Polecat: p.Name, At: now}
	if hb != nil {
		d.Bead = hb.Bead
	}

	if p.SessionRunning {
		switch {
		case hb == nil:
			// A session with no heartbeat is either seconds old or was started
			// outside Gas Town. Neither is a health finding.
			d.Action, d.Reason = Observe, "no-heartbeat-yet"
			d.Basis = "session_running=true, no heartbeat file"
		case hb.State == "exiting":
			// The agent has said it is on its way out. Nuking now would race
			// its own cleanup, which is how a half-pushed branch happens.
			d.Action, d.Reason = Observe, "exiting"
			d.Basis = "heartbeat state=exiting" + contextSuffix(hb)
		default:
			silence := now.Sub(hb.Timestamp)
			d.Silence = silence.Round(time.Second).String()
			if silence > stallAfter {
				d.Action, d.Reason = Ambiguous, "silent-session"
				d.Basis = fmt.Sprintf("session_running=true, heartbeat state=%s, silent for %s (threshold %s)%s",
					hb.State, d.Silence, stallAfter, contextSuffix(hb))
			} else {
				d.Action, d.Reason = Observe, "working"
				d.Basis = fmt.Sprintf("session_running=true, heartbeat state=%s, last seen %s ago",
					hb.State, d.Silence)
			}
		}
		return d
	}

	// From here the session is gone. What matters is only whether anything of
	// value is still sitting in the sandbox.
	switch {
	case p.NeedsRecovery:
		// Checked before SafeToNuke on purpose. The two should never both be
		// true, but if a future Gas Town ever sets them inconsistently, the
		// reading that preserves work must win over the one that destroys it.
		d.Action, d.Reason = Escalate, recoveryReason(p)
		d.Basis = fmt.Sprintf("session_running=false, verdict=%s, reason=%q, cleanup_status=%s, active_mr=%s",
			p.Verdict, p.Reason, p.CleanupStatus, orNone(p.ActiveMR))

	case p.NeedsMQSubmit:
		// Git is clean but the work was never submitted. Submitting it would be
		// publishing code on someone's behalf, which is a decision, not
		// housekeeping.
		d.Action, d.Reason = Escalate, "never-submitted"
		d.Basis = fmt.Sprintf("session_running=false, verdict=%s, branch=%s, active_mr=%s",
			p.Verdict, orNone(p.Branch), orNone(p.ActiveMR))

	case p.SafeToNuke && hasWork(p):
		d.Action, d.Reason = Nuke, "completed"
		d.Basis = fmt.Sprintf("session_running=false, verdict=%s, cleanup_status=%s, branch=%s",
			p.Verdict, p.CleanupStatus, orNone(p.Branch))

	case p.SafeToNuke:
		// Safe to nuke and no evidence it ever did anything: an unused member
		// of a polecat pool. `gt polecat nuke` destroys the agent bead and its
		// CV chain along with the sandbox, so nuking an idle pool identity
		// throws away work history to reclaim nothing.
		d.Action, d.Reason = Observe, "idle-pool-member"
		d.Basis = fmt.Sprintf("session_running=false, verdict=%s, no branch and no active_mr", p.Verdict)

	default:
		d.Action, d.Reason = Observe, "idle"
		d.Basis = fmt.Sprintf("session_running=false, verdict=%s", orNone(p.Verdict))
	}
	return d
}

// recoveryReason keeps Gas Town's own word for what is at risk when it gives
// one, because "git-dirty" and "git-unpushed" call for different human actions
// and flattening them to "needs recovery" loses that.
func recoveryReason(p Polecat) string {
	if p.Reason != "" {
		return p.Reason
	}
	return "needs-recovery"
}

// hasWork reports whether the polecat has anything to show for itself. A branch
// or a merge request is the evidence; the agent bead alone is not, since pooled
// identities have one from creation.
func hasWork(p Polecat) bool {
	return p.Branch != "" || p.ActiveMR != ""
}

func contextSuffix(hb *Heartbeat) string {
	if hb == nil || hb.Context == "" {
		return ""
	}
	return ", context=" + hb.Context
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Tally counts a pass, so that "the witness made no model calls" is something
// we can show rather than assert.
type Tally struct {
	Observed  int `json:"observed"`
	Nuked     int `json:"nuked"`
	Escalated int `json:"escalated"`
	Ambiguous int `json:"ambiguous"`
	Failed    int `json:"failed"`
}

func (t *Tally) Add(a Action) {
	switch a {
	case Observe:
		t.Observed++
	case Nuke:
		t.Nuked++
	case Escalate:
		t.Escalated++
	case Ambiguous:
		t.Ambiguous++
	}
}

// LogArgs renders the tally for slog in the same shape as the reconcile job.
func (t Tally) LogArgs() []any {
	return []any{
		"observed", t.Observed,
		"nuked", t.Nuked,
		"escalated", t.Escalated,
		"ambiguous", t.Ambiguous,
		"failed", t.Failed,
	}
}
