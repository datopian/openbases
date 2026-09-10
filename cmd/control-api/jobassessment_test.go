package main

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// The assessment names the cases that look identical in the raw row.
//
// This is the whole point of the endpoint. A reporter watching
// `status=running, heartbeat_note="256 B of output, last wrote 3m0s ago"` for
// 25 minutes could not tell a working planner from a dead one, because the raw
// fields do not say that a plan job writes beads rather than files, or that a
// heartbeat minutes old means the dispatcher was restarted.
func TestTheAssessmentTellsApartWhatTheRawRowCannot(t *testing.T) {
	now := func(d time.Duration) sql.NullTime {
		return sql.NullTime{Time: time.Now().Add(d), Valid: true}
	}
	none := sql.NullTime{}

	// A restarted dispatcher: `running`, but the heartbeat has stopped. This
	// is the state that had someone waiting on a job that no longer existed.
	got := jobAssessment("plan", "running", "256 B of output", "", now(-25*time.Minute), now(-22*time.Minute), none)
	if !strings.Contains(got, "stale") || !strings.Contains(got, "Re-dispatch") {
		t.Errorf("a stale heartbeat is not called out:\n%s", got)
	}

	// A healthy plan job: quiet, and that is normal. It must NOT be reported
	// as stuck, or the advice becomes "re-file", which is what cost money.
	got = jobAssessment("plan", "running", "256 B of output", "", now(-2*time.Minute), now(-20*time.Second), none)
	if strings.Contains(got, "stale") {
		t.Errorf("a live plan job was called stale:\n%s", got)
	}
	if !strings.Contains(got, "BEADS") {
		t.Errorf("it does not explain that a plan job writes beads, which is why it "+
			"looks idle:\n%s", got)
	}

	// Claimed but never alive: the dispatcher died between claiming and
	// running, which is a different fix from a slow run.
	got = jobAssessment("work", "running", "", "", now(-5*time.Minute), none, none)
	if !strings.Contains(got, "never reported it alive") {
		t.Errorf("a claim with no heartbeat is not distinguished:\n%s", got)
	}

	// Interrupted by a deploy is not broken work.
	got = jobAssessment("work", "failed", "", "the dispatcher was stopped (deploy or restart)", now(-9*time.Minute), now(-8*time.Minute), now(-8*time.Minute))
	if !strings.Contains(got, "Interrupted") && !strings.Contains(got, "interrupted") {
		t.Errorf("a deploy-interrupted run reads as a failure:\n%s", got)
	}
	if strings.Contains(got, "Nothing is wrong") == false {
		t.Errorf("it does not say the work is fine:\n%s", got)
	}

	// Stalled is its own thing: alive and idle, not crashed.
	got = jobAssessment("work", "failed", "", "stalled: nothing written for 10m1s", now(-11*time.Minute), now(-11*time.Minute), now(-1*time.Minute))
	if !strings.Contains(got, "producing nothing") {
		t.Errorf("a stall is not distinguished from a crash:\n%s", got)
	}

	// A finished plan job with no beads is a real outcome, said plainly.
	got = jobAssessment("plan", "done", "", "", now(-10*time.Minute), now(-6*time.Minute), now(-5*time.Minute))
	if !strings.Contains(got, "zero beads") {
		t.Errorf("it does not say that finishing with no beads is an outcome:\n%s", got)
	}

	// And a finished work job does not claim its change landed.
	got = jobAssessment("work", "done", "", "", now(-10*time.Minute), now(-6*time.Minute), now(-5*time.Minute))
	if !strings.Contains(got, "closed_unlanded") {
		t.Errorf("it conflates finishing with landing:\n%s", got)
	}

	// Queued says nothing has been spent, which is the question people ask.
	if got := jobAssessment("plan", "queued", "", "", none, none, none); !strings.Contains(got, "spent") {
		t.Errorf("queued does not say nothing has been spent:\n%s", got)
	}
}
