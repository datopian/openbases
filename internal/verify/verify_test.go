package verify

import (
	"context"
	"testing"

	"github.com/datopian/openbases/internal/jev"
)

// fakeJev answers whatever the test sets, keyed by question key, and records the
// request so a test can assert the questions that were asked.
type fakeJev struct {
	answers map[string]jev.Answer
	model   string
	gotReq  jev.Request
	err     error
}

func (f *fakeJev) Evaluate(_ context.Context, r jev.Request) (jev.Response, error) {
	f.gotReq = r
	if f.err != nil {
		return jev.Response{}, f.err
	}
	return jev.Response{Answers: f.answers, Model: f.model}, nil
}

func f64(v float64) *float64 { return &v }

// A delivery that meets every criterion and scores at the top passes, and the
// questions asked are one Noul per criterion plus the overall Score.
func TestPassWhenAllCriteriaMetAndOverallHigh(t *testing.T) {
	fake := &fakeJev{
		model: "jev-x",
		answers: map[string]jev.Answer{
			"c0":      {Noul: f64(0.9)},
			"c1":      {Noul: f64(0.8)},
			"overall": {Score: f64(2.0), Confidence: f64(0.88)}, // top of a 0..2 rubric
		},
	}
	v, err := Evaluate(context.Background(), fake, Input{
		Acceptance:   "Migration applied; typecheck passes",
		ChangedFiles: []string{"prisma/schema.prisma"},
		DiffSummary:  "added models",
	}, Thresholds{})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Pass {
		t.Errorf("expected pass, got %+v", v)
	}
	if len(v.Criteria) != 2 {
		t.Fatalf("expected 2 criteria, got %d", len(v.Criteria))
	}
	if v.OverallScore != 1.0 {
		t.Errorf("overall fraction = %v, want 1.0", v.OverallScore)
	}
	// One Noul per criterion + the overall Score.
	nouls, scores := 0, 0
	for _, q := range fake.gotReq.Questions {
		switch q.Type {
		case jev.TypeNoul:
			nouls++
		case jev.TypeScore:
			scores++
		}
	}
	if nouls != 2 || scores != 1 {
		t.Errorf("asked %d nouls and %d scores, want 2 and 1", nouls, scores)
	}
}

// One criterion below the threshold fails the whole verdict, even if the overall
// score is high.
func TestFailWhenOneCriterionUnmet(t *testing.T) {
	fake := &fakeJev{answers: map[string]jev.Answer{
		"c0":      {Noul: f64(0.9)},
		"c1":      {Noul: f64(0.2)}, // not met
		"overall": {Score: f64(1.8)},
	}}
	v, err := Evaluate(context.Background(), fake, Input{Acceptance: "does A\ndoes B"}, Thresholds{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Pass {
		t.Errorf("expected fail when a criterion is unmet, got pass: %+v", v)
	}
	if v.Criteria[1].Met {
		t.Error("criterion 1 should be unmet")
	}
}

// All criteria met but a low overall score still fails.
func TestFailWhenOverallLow(t *testing.T) {
	fake := &fakeJev{answers: map[string]jev.Answer{
		"c0":      {Noul: f64(0.9)},
		"overall": {Score: f64(0.5)}, // 0.25 fraction, below default 0.66
	}}
	v, _ := Evaluate(context.Background(), fake, Input{Acceptance: "does A"}, Thresholds{})
	if v.Pass {
		t.Errorf("expected fail on low overall, got %+v", v)
	}
}

// A bead with no acceptance criteria is unverifiable, recorded as NoCriteria and
// not a pass, and Jev is not even called.
func TestNoCriteriaIsRecordedNotPassed(t *testing.T) {
	fake := &fakeJev{}
	v, err := Evaluate(context.Background(), fake, Input{Acceptance: "   \n  "}, Thresholds{})
	if err != nil {
		t.Fatal(err)
	}
	if !v.NoCriteria || v.Pass {
		t.Errorf("empty acceptance should be NoCriteria and not pass: %+v", v)
	}
	if len(fake.gotReq.Questions) != 0 {
		t.Error("Jev should not be called when there is nothing to verify")
	}
}

func TestSplitCriteria(t *testing.T) {
	got := SplitCriteria("- Migration applied\n- Models match spec; typecheck passes\n\n1. Backfill done")
	want := []string{"Migration applied", "Models match spec", "typecheck passes", "Backfill done"}
	if len(got) != len(want) {
		t.Fatalf("split into %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("criterion %d = %q, want %q", i, got[i], want[i])
		}
	}
}
