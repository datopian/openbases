package cost

import (
	"errors"
	"math/big"
	"testing"
)

// The failure this package exists to prevent: a month of real spend totalling
// zero because every individual request rounds to nothing in integer cents.
func TestSubCentCostsSurvive(t *testing.T) {
	// The cost of a real Haiku request, taken from the staging gateway.
	r, err := FromEntry(Entry{
		ID: "01M07AXPNGA68FWY5665HJDDTT", CreatedAt: "2026-08-17T07:45:36Z",
		Cost: 8.8e-05,
	}, "workgraph-staging-oss")
	if err != nil {
		t.Fatal(err)
	}

	got, _, err := big.ParseFloat(r.CostCents, 10, 80, big.ToNearestEven)
	if err != nil {
		t.Fatalf("cost %q is not a number: %v", r.CostCents, err)
	}
	if got.Sign() == 0 {
		t.Fatalf("a real request cost rounded to zero: %q", r.CostCents)
	}

	// $0.000088 is 0.0088 cents.
	want := big.NewFloat(0.0088)
	diff := new(big.Float).Sub(got, want)
	if diff.Abs(diff).Cmp(big.NewFloat(1e-9)) > 0 {
		t.Fatalf("cost = %s cents, want about 0.0088", r.CostCents)
	}
}

// And the aggregate, which is where integer rounding did its damage: many small
// requests must sum to something, not to nothing.
func TestManySmallCostsSumToSomething(t *testing.T) {
	total := new(big.Float).SetPrec(80)
	for i := 0; i < 2182; i++ { // the Haiku request count measured on staging
		r, err := FromEntry(Entry{ID: "x", CreatedAt: "2026-08-17T07:45:36Z", Cost: 8.8e-05}, "g")
		if err != nil {
			t.Fatal(err)
		}
		c, _, _ := big.ParseFloat(r.CostCents, 10, 80, big.ToNearestEven)
		total.Add(total, c)
	}
	// 2182 * 0.0088 cents = 19.2 cents.
	if total.Cmp(big.NewFloat(19)) < 0 {
		t.Fatalf("2182 real requests summed to %s cents; integer rounding would give 0", total.Text('f', 4))
	}
}

func TestAttribution(t *testing.T) {
	r, err := FromEntry(Entry{
		ID: "a", CreatedAt: "2026-08-17T07:45:36Z",
		Metadata: map[string]any{"role": "polecat", "cell": "oss", "rig": "sandbox"},
	}, "g")
	if err != nil {
		t.Fatal(err)
	}
	if r.Role != "polecat" || r.Cell != "oss" || r.Rig != "sandbox" {
		t.Fatalf("attribution lost: %+v", r)
	}

	// Untagged must stay empty rather than acquiring a default. Most historical
	// traffic is untagged, and a wrong attribution is harder to notice than a
	// missing one.
	r, err = FromEntry(Entry{ID: "b", CreatedAt: "2026-08-17T07:45:36Z"}, "g")
	if err != nil {
		t.Fatal(err)
	}
	if r.Role != "" || r.Cell != "" || r.Rig != "" {
		t.Fatalf("an untagged entry acquired attribution: %+v", r)
	}
}

// Cached and failed requests are recorded, not dropped. A cached hit is the
// evidence the cache is working; a failed request often cost money before it
// failed, which is exactly the spend worth finding.
func TestCachedAndFailedAreRecorded(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    Entry
	}{
		{"cached", Entry{ID: "c", CreatedAt: "2026-08-17T07:45:36Z", Cached: true, Success: true}},
		{"failed", Entry{ID: "d", CreatedAt: "2026-08-17T07:45:36Z", Cost: 1e-4, Success: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := FromEntry(tc.e, "g")
			if err != nil {
				t.Fatalf("dropped a %s entry: %v", tc.name, err)
			}
			if r.Cached != tc.e.Cached || r.Succeeded != tc.e.Success {
				t.Fatalf("flags lost: %+v", r)
			}
		})
	}
}

// No id means no idempotency key, so importing it twice would duplicate it.
func TestEntryWithoutIDIsRefused(t *testing.T) {
	_, err := FromEntry(Entry{CreatedAt: "2026-08-17T07:45:36Z"}, "g")
	var skip ErrSkip
	if !errors.As(err, &skip) {
		t.Fatalf("an entry with no id was accepted: %v", err)
	}
}

func TestUnparseableTimestampIsAnError(t *testing.T) {
	if _, err := FromEntry(Entry{ID: "e", CreatedAt: "not a time"}, "g"); err == nil {
		t.Fatal("an unparseable created_at was accepted")
	}
}
