package witness

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func hb(state string, age time.Duration, now time.Time) *Heartbeat {
	return &Heartbeat{Timestamp: now.Add(-age), State: state}
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		polecat Polecat
		beat    *Heartbeat
		want    Action
		reason  string
	}{
		{
			name:    "a working session is left alone",
			polecat: Polecat{Rig: "sandbox", Name: "nitro", SessionRunning: true},
			beat:    hb("working", 30*time.Second, now),
			want:    Observe,
			reason:  "working",
		},
		{
			name:    "a long build is not a stall",
			polecat: Polecat{Rig: "sandbox", Name: "nitro", SessionRunning: true},
			beat:    hb("working", 11*time.Minute, now),
			want:    Observe,
			reason:  "working",
		},
		{
			name:    "a session silent past the threshold is the one ambiguous case",
			polecat: Polecat{Rig: "sandbox", Name: "nitro", SessionRunning: true},
			beat:    hb("working", 25*time.Minute, now),
			want:    Ambiguous,
			reason:  "silent-session",
		},
		{
			name:    "an agent on its way out is not interrupted",
			polecat: Polecat{Rig: "sandbox", Name: "furiosa", SessionRunning: true},
			beat:    &Heartbeat{Timestamp: now.Add(-time.Hour), State: "exiting", Context: "gt done"},
			want:    Observe,
			reason:  "exiting",
		},
		{
			name:    "a session that has not written a heartbeat yet is too young to judge",
			polecat: Polecat{Rig: "sandbox", Name: "rust", SessionRunning: true},
			beat:    nil,
			want:    Observe,
			reason:  "no-heartbeat-yet",
		},
		{
			name: "a finished polecat is nuked",
			polecat: Polecat{
				Rig: "sandbox", Name: "chrome", Verdict: "SAFE_TO_NUKE",
				CleanupStatus: "clean", Branch: "polecat/chrome/sa-ppl@msvpiam8",
				SafeToNuke: true,
			},
			want:   Nuke,
			reason: "completed",
		},
		{
			name: "an unused pool identity is not nuked",
			polecat: Polecat{
				Rig: "sandbox", Name: "spare", Verdict: "SAFE_TO_NUKE", SafeToNuke: true,
			},
			want:   Observe,
			reason: "idle-pool-member",
		},
		{
			name: "uncommitted work is escalated, never destroyed",
			polecat: Polecat{
				Rig: "sandbox", Name: "dust", Verdict: "NEEDS_RECOVERY", Reason: "git-dirty",
				ActiveMR: "sa-wisp-5bn", NeedsRecovery: true,
			},
			want:   Escalate,
			reason: "git-dirty",
		},
		{
			name: "unpushed work keeps Gas Town's own word for it",
			polecat: Polecat{
				Rig: "sandbox", Name: "fury", Verdict: "NEEDS_RECOVERY", Reason: "git-unpushed",
				NeedsRecovery: true,
			},
			want:   Escalate,
			reason: "git-unpushed",
		},
		{
			name: "work that was never submitted is escalated rather than submitted",
			polecat: Polecat{
				Rig: "sandbox", Name: "shiny", Verdict: "NEEDS_MQ_SUBMIT",
				Branch: "polecat/shiny/sa-aaa@x", NeedsMQSubmit: true,
			},
			want:   Escalate,
			reason: "never-submitted",
		},
		{
			// The two flags should never disagree. If a future Gas Town lets
			// them, the reading that preserves work has to win.
			name: "when recovery and nuke disagree, the safe reading wins",
			polecat: Polecat{
				Rig: "sandbox", Name: "thunder", Branch: "polecat/thunder/sa-bbb@y",
				SafeToNuke: true, NeedsRecovery: true, Reason: "git-dirty",
			},
			want:   Escalate,
			reason: "git-dirty",
		},
		{
			name:    "a dormant polecat with nothing at risk is left alone",
			polecat: Polecat{Rig: "sandbox", Name: "guzzle", State: "idle"},
			want:    Observe,
			reason:  "idle",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.polecat, tc.beat, now, StallAfter)
			if got.Action != tc.want {
				t.Errorf("action = %q, want %q (basis: %s)", got.Action, tc.want, got.Basis)
			}
			if got.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.reason)
			}
			if got.Basis == "" {
				t.Error("every decision must carry its basis; an unexplained health action cannot be reviewed")
			}
		})
	}
}

// TestDecideNeverNukesWorkAtRisk states the property the table above only
// samples. Nuke destroys the worktree, the branch and the agent bead, so the
// one thing that must hold for every possible input is that it is never chosen
// while Gas Town says something could be lost.
func TestDecideNeverNukesWorkAtRisk(t *testing.T) {
	now := time.Now()
	for _, recovery := range []bool{true, false} {
		for _, submit := range []bool{true, false} {
			for _, safe := range []bool{true, false} {
				for _, running := range []bool{true, false} {
					p := Polecat{
						Rig: "sandbox", Name: "x", Branch: "polecat/x/sa-1@a",
						NeedsRecovery: recovery, NeedsMQSubmit: submit,
						SafeToNuke: safe, SessionRunning: running,
					}
					d := Decide(p, nil, now, StallAfter)
					if d.Action == Nuke && (recovery || submit || running) {
						t.Fatalf("nuked with recovery=%v submit=%v running=%v: %s",
							recovery, submit, running, d.Basis)
					}
				}
			}
		}
	}
}

// TestDedupeKeyDistinguishesSituations guards the inbox against both failure
// modes: one row per tick for a standing problem, and one row silently reused
// when the problem changes.
func TestDedupeKeyDistinguishesSituations(t *testing.T) {
	a := Decision{Rig: "sandbox", Polecat: "dust", Reason: "git-dirty"}
	b := Decision{Rig: "sandbox", Polecat: "dust", Reason: "git-dirty", At: time.Now()}
	c := Decision{Rig: "sandbox", Polecat: "dust", Reason: "never-submitted"}
	d := Decision{Rig: "autoclaw", Polecat: "dust", Reason: "git-dirty"}

	if a.DedupeKey() != b.DedupeKey() {
		t.Error("the same situation observed twice must collapse to one item")
	}
	if a.DedupeKey() == c.DedupeKey() {
		t.Error("a different problem with the same polecat must raise its own item")
	}
	if a.DedupeKey() == d.DedupeKey() {
		t.Error("the same polecat name in another rig is another polecat")
	}
}

// TestDecideAgainstRealOutput runs the decision over a real `gt polecat list
// sandbox --json` payload captured from the staging execution node. A fixture
// invented from the documentation would agree with whatever the parser happens
// to do; this one caught that Gas Town reports a live-looking `state: idle`
// alongside `session_running: false`, so `state` is not the liveness signal.
func TestDecideAgainstRealOutput(t *testing.T) {
	raw, err := os.ReadFile("testdata/polecat_list.json")
	if err != nil {
		t.Fatal(err)
	}
	var pcs []Polecat
	if err := json.Unmarshal(raw, &pcs); err != nil {
		t.Fatalf("the captured payload no longer parses: %v", err)
	}
	if len(pcs) == 0 {
		t.Fatal("fixture is empty")
	}

	var tally Tally
	for _, p := range pcs {
		if p.Rig == "" || p.Name == "" {
			t.Fatalf("payload entry without rig or name: %+v", p)
		}
		d := Decide(p, nil, time.Now(), StallAfter)
		tally.Add(d.Action)
	}

	// Every one of these polecats was left dirty by earlier testing, so the
	// pass should be entirely escalations — and, importantly, no nukes.
	if tally.Nuked != 0 {
		t.Errorf("nuked %d polecats with uncommitted work", tally.Nuked)
	}
	if tally.Escalated == 0 {
		t.Error("expected the recorded dirty sandboxes to escalate")
	}
	if tally.Ambiguous != 0 {
		t.Errorf("no session was running, so nothing should be ambiguous; got %d", tally.Ambiguous)
	}
}
