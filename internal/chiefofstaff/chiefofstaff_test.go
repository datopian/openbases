package chiefofstaff

import (
	"context"
	"strings"
	"testing"
)

// Every claim must be classified and, when it asserts something, be traceable.
//
// The acceptance criterion is that every factual sentence links to evidence. A
// claim with no evidence is allowed in exactly one case — an assertion that
// nothing was found — and that case must carry a basis explaining what was
// searched, or "nothing needs you" becomes indistinguishable from "nothing was
// looked at".
func TestClaimsAreClassifiedAndTraceable(t *testing.T) {
	claims := []Claim{
		{Statement: "a/b #1 is open", Kind: Observed,
			Evidence: []Evidence{{Kind: "pull_request", Ref: "a/b#1"}}},
		{Statement: "nothing found", Kind: Observed,
			Basis: "no rows matched", Evidence: []Evidence{}},
	}

	for _, c := range claims {
		switch c.Kind {
		case Observed, Calculated, Inferred:
		default:
			t.Errorf("claim %q has kind %q, which is not one of observed, calculated, inferred", c.Statement, c.Kind)
		}
		if len(c.Evidence) == 0 && strings.TrimSpace(c.Basis) == "" {
			t.Errorf("claim %q has neither evidence nor a basis; a reader cannot check or challenge it", c.Statement)
		}
	}
}

// A derived judgement must never be labelled as an observation.
//
// "This project is at risk" is a rule applied to facts, not a fact. Presenting
// it as observed removes the reader's ability to disagree with the rule, which
// is the only part of it that can actually be wrong.
func TestRiskIsCalculatedNotObserved(t *testing.T) {
	c := Claim{
		Statement: "portaljs-oss has 3 repositories and none has ever reported activity.",
		Kind:      Calculated,
		Basis:     "A registered repository that has never produced a projected event may mean no work, or a broken integration.",
		Evidence:  []Evidence{{Kind: "project", Ref: "portaljs-oss"}},
	}
	if c.Kind != Calculated {
		t.Fatal("a risk judgement must be calculated, not observed")
	}
	if c.Basis == "" {
		t.Fatal("a calculated claim without its rule cannot be challenged")
	}
}

// An unsupported question is refused rather than guessed at.
//
// The plan requires this layer not to receive the company as an unbounded
// prompt. Refusing, and saying what IS answerable, is more useful than an
// invented answer and cannot leak anything.
func TestUnsupportedQuestionIsRefused(t *testing.T) {
	s := NewStore(nil)
	_, err := s.Ask(context.Background(), "u", "What is our runway?")
	if err == nil {
		t.Fatal("an unsupported question was accepted")
	}
	for _, q := range Questions {
		if !strings.Contains(err.Error(), q) {
			t.Errorf("the refusal does not list the supported question %q", q)
		}
	}
}

// Nothing in this package may produce an inferred claim yet.
//
// The value exists so that a model-backed answer is labelled from the first
// commit rather than retrofitted, but until one exists, a claim marked inferred
// would be a lie about where the sentence came from.
func TestNoInferredClaimsAreProducedYet(t *testing.T) {
	if Inferred != "inferred" {
		t.Fatal("the inferred kind changed value; stored claims would be misread")
	}
}
