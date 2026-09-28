// Package verify judges whether a delivered change meets a bead's acceptance
// criteria, using Jev (ADR-0029).
//
// It is the second half of the acceptance gate: internal/jev is the transport,
// this is the question. Given a bead's acceptance criteria and a summary of what
// was delivered, it asks Jev one Noul per criterion ("does the delivered change
// satisfy this?") plus one overall Score, and returns a structured verdict with
// per-criterion confidence and a pass/fail at a configurable threshold.
//
// It does not read diffs or reach the node: the caller builds the summary within
// Jev's 32k context and passes it in. It does not gate anything either -- shadow
// mode records the verdict; enforcement is a later decision once the thresholds
// are calibrated against real beads.
package verify

import (
	"context"
	"fmt"
	"strings"

	"github.com/datopian/openbases/internal/jev"
)

// Evaluator is the slice of internal/jev this package needs, so a test can drive
// it with a fake and the real client (*jev.Client) satisfies it unchanged.
type Evaluator interface {
	Evaluate(ctx context.Context, r jev.Request) (jev.Response, error)
}

// Thresholds decide pass/fail. Conservative defaults; shadow mode exists to tune
// them before they gate anything.
type Thresholds struct {
	// Criterion is the minimum Noul for one criterion to count as met.
	Criterion float64
	// Overall is the minimum overall score, as a fraction of the rubric's top,
	// for the delivery to pass.
	Overall float64
}

// DefaultThresholds are used when a caller passes the zero value.
var DefaultThresholds = Thresholds{Criterion: 0.6, Overall: 0.66}

// Input is what to judge. The caller supplies an already-bounded summary.
type Input struct {
	// Acceptance is the bead's acceptance criteria, as written. Split into
	// individual criteria here.
	Acceptance string
	// ChangedFiles is the list of paths the delivery touched.
	ChangedFiles []string
	// DiffSummary is a bounded description of the change (a stat, key hunks, or a
	// summary), NOT the whole patch -- Jev's context is 32k tokens.
	DiffSummary string
	// ClosingComment is what the agent said when it closed the bead.
	ClosingComment string
}

// CriterionVerdict is one acceptance criterion's result.
type CriterionVerdict struct {
	Text string
	// Noul is Jev's calibrated truth that the delivery satisfies this criterion.
	Noul float64
	Met  bool
}

// Verdict is the whole judgement.
type Verdict struct {
	Criteria          []CriterionVerdict
	OverallScore      float64 // as a fraction of the rubric top, 0..1
	OverallConfidence float64
	Pass              bool
	// NoCriteria is true when the bead had no acceptance criteria to judge
	// against -- itself a finding (an unverifiable bead), recorded rather than
	// treated as a pass.
	NoCriteria bool
	Model      string
}

// the rubric labels for the overall score, lowest first. The overall fraction is
// the returned score divided by (len-1).
var overallLabels = []string{
	"does not meet the acceptance criteria",
	"partially meets the acceptance criteria",
	"fully meets the acceptance criteria",
}

// Evaluate asks Jev whether the delivery meets each criterion and overall.
func Evaluate(ctx context.Context, ev Evaluator, in Input, th Thresholds) (Verdict, error) {
	if th.Criterion == 0 {
		th.Criterion = DefaultThresholds.Criterion
	}
	if th.Overall == 0 {
		th.Overall = DefaultThresholds.Overall
	}

	criteria := SplitCriteria(in.Acceptance)
	if len(criteria) == 0 {
		// Nothing to verify against. Recorded as a distinct outcome rather than a
		// pass -- a bead that closes with no checkable acceptance is exactly what
		// this gate is meant to surface.
		return Verdict{NoCriteria: true, Pass: false}, nil
	}

	state := map[string]any{
		"acceptance_criteria": in.Acceptance,
		"changed_files":       in.ChangedFiles,
		"change_summary":      in.DiffSummary,
		"closing_comment":     in.ClosingComment,
	}

	questions := make([]jev.Question, 0, len(criteria)+1)
	keyFor := make(map[string]int, len(criteria))
	for i, crit := range criteria {
		key := fmt.Sprintf("c%d", i)
		keyFor[key] = i
		questions = append(questions, jev.Noul(key,
			"The delivered change (see changed_files and change_summary) satisfies "+
				"this acceptance criterion: "+crit))
	}
	questions = append(questions, jev.Score("overall",
		"Overall, how well does the delivered change satisfy the bead's acceptance criteria?",
		overallLabels))

	resp, err := ev.Evaluate(ctx, jev.Request{State: state, Questions: questions})
	if err != nil {
		return Verdict{}, err
	}

	v := Verdict{Model: resp.Model, Criteria: make([]CriterionVerdict, len(criteria))}
	allMet := true
	for key, i := range keyFor {
		ans, ok := resp.Answers[key]
		cv := CriterionVerdict{Text: criteria[i]}
		if ok && ans.Noul != nil {
			cv.Noul = *ans.Noul
			cv.Met = cv.Noul >= th.Criterion
		}
		if !cv.Met {
			allMet = false
		}
		v.Criteria[i] = cv
	}

	if overall, ok := resp.Answers["overall"]; ok {
		if overall.Score != nil {
			top := float64(len(overallLabels) - 1)
			if top > 0 {
				v.OverallScore = clamp(*overall.Score/top, 0, 1)
			}
		}
		if overall.Confidence != nil {
			v.OverallConfidence = *overall.Confidence
		}
	}

	v.Pass = allMet && v.OverallScore >= th.Overall
	return v, nil
}

// SplitCriteria breaks acceptance text into individual, checkable criteria.
//
// Acceptance is written many ways -- one sentence, a bulleted list, a
// semicolon-separated line. This splits on lines and on bullet/semicolon
// separators, strips list markers, and drops blanks, so each Noul asks about one
// thing. Over-splitting is cheaper than under-splitting: an extra atomic question
// costs almost nothing and a compound one hides a half-met criterion.
func SplitCriteria(acceptance string) []string {
	var out []string
	for _, line := range strings.Split(acceptance, "\n") {
		// A single line may still carry several criteria joined by ';'.
		for _, part := range strings.Split(line, ";") {
			c := strings.TrimSpace(part)
			c = strings.TrimLeft(c, "-*•0123456789.() \t")
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			out = append(out, c)
		}
	}
	return out
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
