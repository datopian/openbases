// Package cost turns AI Gateway log entries into durable usage records.
//
// Spend currently exists only in Cloudflare's gateway logs. Those rotate on a
// DELETE_OLDEST policy and cannot be joined to a project or a bead, so every
// cost question is answered by a script calling an API that will eventually
// forget — and the answer cannot be checked later, because the evidence is gone.
//
// The mapping from a log entry to a record is a pure function, separated from
// both the fetching and the writing, for the same reason the witness and the
// monitor are built that way: the interesting cases are awkward to arrange
// against a live gateway and trivial as table-driven tests. Cached requests,
// failed requests, untagged requests and sub-cent costs are all decisions, and
// all of them are wrong by default in an obvious implementation.
package cost

import (
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Entry is one AI Gateway log row, as the API returns it.
type Entry struct {
	ID        string         `json:"id"`
	CreatedAt string         `json:"created_at"`
	Provider  string         `json:"provider"`
	Model     string         `json:"model"`
	TokensIn  int64          `json:"tokens_in"`
	TokensOut int64          `json:"tokens_out"`
	Cost      float64        `json:"cost"`
	Cached    bool           `json:"cached"`
	Success   bool           `json:"success"`
	Metadata  map[string]any `json:"metadata"`
}

// Record is what gets written, one row per gateway log entry.
type Record struct {
	ExternalID string
	Gateway    string
	Provider   string
	Model      string
	InputTok   int64
	OutputTok  int64
	// CostCents as a decimal string, not a float.
	//
	// A typical request costs $0.000088. Rounded to integer cents that is zero,
	// and a month of real spend totals zero — a report that confidently says
	// nothing was spent, which is worse than no report. Carried as a string so
	// the value handed to PostgreSQL's numeric is exactly what was computed,
	// with no second float conversion in the driver to argue about.
	CostCents string
	Cached    bool
	Succeeded bool
	Role      string
	Cell      string
	Rig       string
	Occurred  time.Time
}

// ErrSkip reports an entry that is deliberately not recorded.
type ErrSkip struct{ Reason string }

func (e ErrSkip) Error() string { return "skipped: " + e.Reason }

// FromEntry maps one log entry to a record.
//
// Returns ErrSkip for entries that must not become rows, which is a decision
// rather than a filter and is listed here so it can be argued with.
func FromEntry(e Entry, gateway string) (Record, error) {
	if strings.TrimSpace(e.ID) == "" {
		// Without the gateway's id there is no idempotency key, so a re-run
		// would duplicate it. Refusing is better than importing something that
		// cannot be imported twice.
		return Record{}, ErrSkip{Reason: "entry has no id"}
	}

	occurred, err := time.Parse(time.RFC3339, e.CreatedAt)
	if err != nil {
		return Record{}, fmt.Errorf("entry %s has an unparseable created_at %q: %w", e.ID, e.CreatedAt, err)
	}

	role, cell, rig := attribution(e.Metadata)

	return Record{
		ExternalID: e.ID,
		Gateway:    gateway,
		Provider:   e.Provider,
		Model:      e.Model,
		InputTok:   e.TokensIn,
		OutputTok:  e.TokensOut,
		CostCents:  centsOf(e.Cost),
		// Cached and failed requests ARE recorded, deliberately.
		//
		// A cached hit costs nothing and is the strongest evidence the cache is
		// working; dropping it makes the saving invisible. A failed request often
		// costs money — the tokens were spent before the failure — and dropping
		// it hides spend that produced no result, which is exactly the spend
		// worth finding. Both are flagged so a report can exclude them; neither
		// is silently discarded here.
		Cached:    e.Cached,
		Succeeded: e.Success,
		Role:      role,
		Cell:      cell,
		Rig:       rig,
		Occurred:  occurred,
	}, nil
}

// centsOf converts dollars to cents without going through a float.
//
// big.Float, because the arithmetic is money and the input is already a float64
// that cannot represent 0.000088 exactly. Multiplying by 100 in float64 and
// formatting compounds that error across hundreds of thousands of rows; doing it
// in arbitrary precision and formatting once does not.
func centsOf(dollars float64) string {
	cents := new(big.Float).SetPrec(80).SetFloat64(dollars)
	cents.Mul(cents, big.NewFloat(100))
	return cents.Text('f', 8)
}

// attribution reads role, cell and rig out of cf-aig-metadata.
//
// Everything is optional and stays empty when absent. Most historical traffic
// carries no metadata at all — 98.3% of it, measured on the staging oss gateway
// — and an empty value that means "we do not know" is worth more than a default
// that quietly attributes the spend to somebody.
func attribution(md map[string]any) (role, cell, rig string) {
	if md == nil {
		return "", "", ""
	}
	get := func(k string) string {
		v, ok := md[k]
		if !ok || v == nil {
			return ""
		}
		s, ok := v.(string)
		if !ok {
			// Numbers and booleans are rendered rather than dropped: a caller
			// that tagged rig=3 meant something by it.
			return strings.TrimSpace(fmt.Sprint(v))
		}
		return strings.TrimSpace(s)
	}
	return get("role"), get("cell"), get("rig")
}
