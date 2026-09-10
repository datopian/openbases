package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// jobAssessment says what a job's state MEANS, in one sentence.
//
// The fields it reads were all being served already. What was missing is the
// reading of them: a filing job that returns {status: queued} and then shows
// `status=running, heartbeat_note="256 B of output, last wrote 3m0s ago"` is
// telling a person almost nothing unless they also know that a PLAN job writes
// beads rather than files, so its progress signals are not the ones a work job
// produces. Someone with that gap spent 25 minutes unable to tell a working
// job from a dead one, and re-filed a reworded brief to find out.
//
// It is deliberately plain prose rather than a code. A caller that wants to
// branch has status, heartbeat_at and finished_at; this is for the human who
// asked "what is it doing".
func jobAssessment(kind, status, note, result string,
	claimed, beat, finished sql.NullTime) string {

	switch status {
	case "queued":
		return "queued: no node has claimed it yet. Nothing is running and nothing " +
			"has been spent."

	case "running":
		// The heartbeat is the only live evidence, and its absence is itself
		// evidence: the node updates it every thirty seconds while a run is
		// alive, so a claim with no heartbeat means the dispatcher never got
		// far enough to send one.
		if !beat.Valid {
			return "claimed, but the node has never reported it alive. Either it has " +
				"only just started, or the dispatcher died between claiming and " +
				"running it -- if this persists for more than a minute, treat it as " +
				"the second."
		}
		idle := time.Since(beat.Time).Round(time.Second)
		if idle > 3*time.Minute {
			return fmt.Sprintf("says running, but the node has not reported it alive "+
				"for %s. A live run reports every 30s, so this row is almost certainly "+
				"stale: the dispatcher was restarted (a deploy does that) and nothing "+
				"closed the row. Re-dispatch it. Last note: %q", idle, note)
		}
		if kind == "plan" {
			return fmt.Sprintf("running for %s and alive %s ago. A planning job writes "+
				"BEADS, not files, and its harness buffers output -- so little output "+
				"and an unchanged checkout are normal here and do not mean it is stuck. "+
				"Check for new beads in the project rather than for progress in this "+
				"note: %q", since(claimed), time.Since(beat.Time).Round(time.Second), note)
		}
		return fmt.Sprintf("running for %s and alive %s ago: %q",
			since(claimed), time.Since(beat.Time).Round(time.Second), note)

	case "failed":
		// The dispatcher writes this sentence into the result when a shutdown
		// interrupts a run, and it is the difference between "your work is
		// wrong" and "a deploy happened".
		if strings.Contains(result, "the dispatcher was stopped") {
			return "interrupted, not failed: the dispatcher was restarted -- a deploy " +
				"does that -- while this was running. Nothing is wrong with the work. " +
				"Re-dispatch it."
		}
		if strings.Contains(result, "stalled: nothing written") {
			return "stopped for producing nothing: no output and no file changed for " +
				"the stall timeout. The agent was alive and idle rather than working. " +
				"Its last output is in `result`."
		}
		return "failed. `result` holds the last of what it printed, which is where " +
			"the reason usually is."

	case "done":
		if kind == "plan" {
			return "finished. A planning job's product is beads, so check the project's " +
				"work list: finishing with zero beads is a real outcome and means the " +
				"brief did not resolve into anything the planner would file."
		}
		return "finished. Whether it LANDED anything is a separate question -- ask about " +
			"the bead, where `outcome` tells `done` from `closed_unlanded`."
	}
	return "unrecognised status " + status + ". This is a bug in the reader, not in the job."
}

// since is the elapsed time from a nullable start, or "an unknown time".
func since(t sql.NullTime) string {
	if !t.Valid {
		return "an unknown time"
	}
	return time.Since(t.Time).Round(time.Second).String()
}
