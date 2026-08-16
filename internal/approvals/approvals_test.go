package approvals

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The digest is what gets approved, so two different actions must never share
// one — and the separator matters: without it, ("ab","c") and ("a","bc") hash
// identically, and an approval for one would unlock the other.
func TestDigestSeparatesActionFromParameters(t *testing.T) {
	a := Digest("ab", []byte("c"))
	b := Digest("a", []byte("bc"))
	if a == b {
		t.Fatal("two different actions produced the same digest; an approval for one would unlock the other")
	}
	if Digest("deploy", []byte(`{"env":"prod"}`)) == Digest("deploy", []byte(`{"env":"staging"}`)) {
		t.Fatal("changing a parameter did not change the digest")
	}
	if got := Digest("deploy", []byte("{}")); len(got) != 64 {
		t.Fatalf("digest is %d chars; the column requires 64 hex", len(got))
	}
	// Stable across calls, or an approval could never be matched to an action.
	if Digest("deploy", []byte("{}")) != Digest("deploy", []byte("{}")) {
		t.Fatal("the digest is not deterministic")
	}
}

// A rejection with no reason is refused before any database work.
//
// "No" without a reason gives the requester nothing to act on, which turns a
// review into a wall.
func TestRejectionRequiresAReason(t *testing.T) {
	s := NewStore(nil)
	err := s.Decide(context.Background(), Decision{
		RequestID: "r", UserID: "u", Approve: false,
	})
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("expected a structured refusal, got %v", err)
	}
	if refusal.Code != "reason_required" {
		t.Errorf("unexpected code %q", refusal.Code)
	}
	if refusal.Remediation == "" {
		t.Error("a refusal with no remediation leaves the caller stuck")
	}
}

// Every refusal must name its policy and say what to do. A bare "forbidden"
// makes the rule invisible outside the code that enforces it.
func TestRefusalsAreActionable(t *testing.T) {
	for _, r := range []*Refusal{
		{Code: "self_approval", Policy: "not_creator",
			Message: "x", AllowedActor: "another role holder", Remediation: "ask a colleague"},
		{Code: "digest_changed", Policy: "action_digest_binding",
			Message: "x", Remediation: "reload"},
	} {
		if r.Policy == "" {
			t.Errorf("%s names no policy", r.Code)
		}
		if r.Remediation == "" {
			t.Errorf("%s offers no remediation", r.Code)
		}
		if !strings.Contains(r.Error(), r.Code) {
			t.Errorf("the error string omits the code: %s", r.Error())
		}
	}
}

func TestDecisionNeedsRequestAndDecider(t *testing.T) {
	s := NewStore(nil)
	for _, d := range []Decision{
		{UserID: "u", Approve: true},
		{RequestID: "r", Approve: true},
	} {
		if err := s.Decide(context.Background(), d); err == nil {
			t.Errorf("accepted an incomplete decision: %+v", d)
		}
	}
}
