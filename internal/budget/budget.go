// Package budget decides whether a piece of work may be dispatched.
//
// The decision is separated from the query for the same reason the witness, the
// monitor and the cost mapping are: the interesting cases — no budget at all,
// spend data an hour old, a ceiling of exactly zero — are awkward to arrange
// against a live database and trivial as table-driven tests. Every one of them
// has an obvious wrong answer.
//
// The hard constraint this package works around is that spend is not live.
// usage_records is filled by an hourly import from the AI Gateway log, so the
// most recent answer is always somewhat behind. A budget check that treats
// hour-old data as current will authorise work that is already over the line,
// and will do it confidently. So staleness is an input to the decision rather
// than a footnote on it.
package budget

import (
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"
)

// Status is what the database knows: which budget applies, and what has been
// spent against it today.
type Status struct {
	// SubjectKind is "bead", "project", "cell", or "none" when no budget exists
	// anywhere up the chain.
	SubjectKind string `json:"subject_kind"`
	SubjectKey  string `json:"subject_key,omitempty"`
	// DailyCents and SpentCents are decimal strings, not floats: they are money,
	// and the values came out of numeric columns that can hold 0.0088.
	DailyCents     string `json:"daily_cents,omitempty"`
	SpentCents     string `json:"spent_cents"`
	RemainingCents string `json:"remaining_cents,omitempty"`
	Exceeded       bool   `json:"exceeded"`
	// StalenessSeconds is how old the least recent gateway import is. Negative
	// means nothing has ever been imported, which is not the same as fresh.
	StalenessSeconds int64 `json:"staleness_seconds"`
	// MaxAgents and MaxRuntimeMinutes are the operational limits the budget
	// carries. Zero means the budget sets none.
	//
	// They are returned so a caller can enforce them, which nothing did for the
	// first two months they existed: wg-budget set them, budget_limits stored
	// them, and no code read either (wg-726). Two numbers that looked like
	// controls and were not.
	MaxAgents         int `json:"max_agents,omitempty"`
	MaxRuntimeMinutes int `json:"max_runtime_minutes,omitempty"`
}

// Decision is the answer, with the reason a person will read at the moment they
// are being told they cannot start work.
type Decision struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason"`
	// Warning is set on an allowed decision that the caller should still see —
	// an allow granted on stale data, or one that is nearly at the ceiling.
	Warning string `json:"warning,omitempty"`
	// MaxAgents and MaxRuntimeMinutes are the governing budget's operational
	// limits, for the caller to enforce where it can see them. Concurrency is a
	// property of the node, not of the control plane, so the control plane can
	// only say what the ceiling is (wg-726).
	MaxAgents         int    `json:"max_agents,omitempty"`
	MaxRuntimeMinutes int    `json:"max_runtime_minutes,omitempty"`
	Status            Status `json:"status"`
}

// Policy is what the operator can tune without a release.
type Policy struct {
	// MaxStaleness is how old the spend data may be and still be trusted. Beyond
	// it, an allow becomes a refusal rather than a warning.
	//
	// Zero means "do not check staleness", which is the right setting for an
	// environment where the import is not running yet — it makes budgets
	// advisory rather than making every dispatch fail for a reason that has
	// nothing to do with budgets.
	MaxStaleness time.Duration
	// AllowUnbudgeted decides what happens to work no budget covers.
	//
	// True by default in the caller, and deliberately so: the alternative is
	// that adding budget enforcement stops all work until somebody enumerates
	// every project, which is how enforcement gets switched off entirely. It is
	// still reported, so "nothing is budgeted" is visible rather than silent.
	AllowUnbudgeted bool
	// WarnAtPercent raises a warning on an allowed decision once spend reaches
	// this share of the ceiling. Zero disables it.
	WarnAtPercent float64
}

// PolicyFromEnv is DefaultPolicy with the thresholds an operator can move.
//
// Configuration rather than constants, for the reason the monitor's thresholds
// are: the right value differs between staging and production, and a threshold
// that needs a release to change is one that gets worked around instead. The
// worked-around version here is --no-budget-check on every dispatch, which
// switches enforcement off entirely rather than loosening it.
//
//	WG_BUDGET_MAX_STALENESS   how old spend data may be, e.g. "2h", "0" to ignore
//	WG_BUDGET_ALLOW_UNBUDGETED  "false" to refuse work no budget covers
//	WG_BUDGET_WARN_PERCENT    warn once this share of a ceiling is spent
func PolicyFromEnv() Policy {
	p := DefaultPolicy()
	if v := os.Getenv("WG_BUDGET_MAX_STALENESS"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			p.MaxStaleness = d
		}
	}
	if v := os.Getenv("WG_BUDGET_ALLOW_UNBUDGETED"); v != "" {
		p.AllowUnbudgeted = !strings.EqualFold(v, "false") && v != "0"
	}
	if v := os.Getenv("WG_BUDGET_WARN_PERCENT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			p.WarnAtPercent = f
		}
	}
	return p
}

// DefaultPolicy is what the dispatcher uses unless told otherwise.
func DefaultPolicy() Policy {
	return Policy{
		// Two hours: the importer runs hourly with a two-hour overlap, so one
		// missed run is still trusted and two are not. A tighter bound would
		// refuse dispatch every time a single import was late, which teaches
		// people to pass --force.
		MaxStaleness:    2 * time.Hour,
		AllowUnbudgeted: true,
		WarnAtPercent:   80,
	}
}

// Decide turns a status into an answer.
func Decide(s Status, p Policy) Decision {
	d := Decision{Status: s}

	// Over the line is over the line, whatever the data's age. Refusing on stale
	// data that already says "exceeded" is right: more spend has happened since,
	// never less.
	if s.Exceeded {
		d.Allow = false
		d.Reason = fmt.Sprintf("the %s budget for %s is spent: %s of %s cents used today",
			s.SubjectKind, s.SubjectKey, trim(s.SpentCents), trim(s.DailyCents))
		return d
	}

	if s.SubjectKind == "none" || s.DailyCents == "" {
		if !p.AllowUnbudgeted {
			d.Allow = false
			d.Reason = "no budget covers this work, and unbudgeted work is not allowed"
			return d
		}
		d.Allow = true
		d.Reason = "no budget covers this work"
		d.Warning = "unbudgeted: set one with wg-budget set, or this spend is bounded only by the gateway pool"
		return d
	}

	// Staleness only matters once there IS a budget and it is not yet spent,
	// because that is the only case where the answer could change under us.
	if p.MaxStaleness > 0 {
		if s.StalenessSeconds < 0 {
			d.Allow = false
			d.Reason = "no spend has ever been imported, so the budget cannot be checked at all"
			return d
		}
		if age := time.Duration(s.StalenessSeconds) * time.Second; age > p.MaxStaleness {
			d.Allow = false
			d.Reason = fmt.Sprintf(
				"spend data is %s old, older than the %s this check trusts; the budget cannot be checked",
				age.Round(time.Minute), p.MaxStaleness)
			return d
		}
	}

	d.Allow = true
	d.Reason = fmt.Sprintf("%s cents of the %s budget for %s remain",
		trim(s.RemainingCents), s.SubjectKind, s.SubjectKey)
	// Carried onto the decision so the caller does not have to reach back into
	// the status for them, and so a transport that only forwards the decision
	// still forwards the limits.
	d.MaxAgents, d.MaxRuntimeMinutes = s.MaxAgents, s.MaxRuntimeMinutes

	if p.WarnAtPercent > 0 {
		if used, ok := percent(s.SpentCents, s.DailyCents); ok && used >= p.WarnAtPercent {
			d.Warning = fmt.Sprintf("%.0f%% of the %s budget for %s is already spent",
				used, s.SubjectKind, s.SubjectKey)
		}
	}
	return d
}

// percent computes spent/limit as a percentage without going through a float
// until the last step. Returns false when the limit is zero, where a percentage
// is not defined — a zero budget is handled by Exceeded, not here.
func percent(spent, limit string) (float64, bool) {
	sp, ok1 := parse(spent)
	li, ok2 := parse(limit)
	if !ok1 || !ok2 || li.Sign() == 0 {
		return 0, false
	}
	q := new(big.Float).SetPrec(80).Quo(sp, li)
	q.Mul(q, big.NewFloat(100))
	out, _ := q.Float64()
	return out, true
}

func parse(s string) (*big.Float, bool) {
	if s == "" {
		return nil, false
	}
	f, _, err := big.ParseFloat(s, 10, 80, big.ToNearestEven)
	if err != nil {
		return nil, false
	}
	return f, true
}

// Cents renders a money string for a person: numeric(16,4) arrives as
// "200.0000", and "200" is what somebody being told to stop needs to read.
func Cents(s string) string { return trim(s) }

// trim renders a money string without a wall of trailing zeroes. numeric(16,4)
// arrives as "200.0000", and "200" is what a person needs to read.
func trim(s string) string {
	f, ok := parse(s)
	if !ok {
		return s
	}
	// Four places is the precision budgets are stored at; anything beyond it is
	// an artefact of the summation rather than a number anybody set.
	out := f.Text('f', 4)
	for len(out) > 0 && out[len(out)-1] == '0' {
		out = out[:len(out)-1]
	}
	if len(out) > 0 && out[len(out)-1] == '.' {
		out = out[:len(out)-1]
	}
	if out == "" || out == "-" {
		return "0"
	}
	return out
}
