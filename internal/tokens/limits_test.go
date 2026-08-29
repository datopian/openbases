package tokens

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestRateLimitAdmitsUpToTheRateThenRefuses(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	l := NewLimiter()
	l.Now = func() time.Time { return now }

	for i := 0; i < 60; i++ {
		if ok, _ := l.Allow("t1", 60); !ok {
			t.Fatalf("request %d of 60 was refused inside the rate", i+1)
		}
	}
	ok, retry := l.Allow("t1", 60)
	if ok {
		t.Fatal("the 61st request in a minute was admitted")
	}
	if retry <= 0 || retry > time.Minute {
		t.Fatalf("Retry-After of %s is not a usable instruction", retry)
	}
}

// A refusal has to tell the caller when to come back, or a well-behaved client
// has no choice but to poll and become the problem it was refused for.
func TestRetryAfterShrinksAsTheWindowElapses(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	l := NewLimiter()
	l.Now = func() time.Time { return now }

	for i := 0; i < 10; i++ {
		l.Allow("t1", 10)
	}
	_, early := l.Allow("t1", 10)

	now = now.Add(45 * time.Second)
	_, late := l.Allow("t1", 10)

	if late >= early {
		t.Fatalf("Retry-After did not shrink: %s then %s", early, late)
	}
}

func TestTheWindowResetsAndTheTokenIsAdmittedAgain(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	l := NewLimiter()
	l.Now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		l.Allow("t1", 5)
	}
	if ok, _ := l.Allow("t1", 5); ok {
		t.Fatal("admitted past the rate")
	}

	now = now.Add(time.Minute)
	if ok, _ := l.Allow("t1", 5); !ok {
		t.Fatal("still refused after the window elapsed")
	}
}

// One noisy tool must not throttle its owner's other tools, which is the whole
// reason the limit is per token rather than per user.
func TestOneTokenExhaustingItsRateDoesNotAffectAnother(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	l := NewLimiter()
	l.Now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		l.Allow("noisy", 5)
	}
	if ok, _ := l.Allow("noisy", 5); ok {
		t.Fatal("the noisy token was admitted past its rate")
	}
	if ok, _ := l.Allow("quiet", 5); !ok {
		t.Fatal("a different token was throttled by the noisy one")
	}
}

// A session has no token id. It must not fall into a shared bucket keyed on the
// empty string, which would throttle every browser user together.
func TestASessionIsNotRateLimited(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 1000; i++ {
		if ok, _ := l.Allow("", 60); !ok {
			t.Fatal("an interactive session was rate limited")
		}
	}
}

func TestSweepDropsStaleWindowsAndKeepsLiveOnes(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	l := NewLimiter()
	l.Now = func() time.Time { return now }

	l.Allow("old", 60)
	now = now.Add(2 * time.Minute)
	l.Allow("fresh", 60)
	l.Sweep()

	l.mu.Lock()
	_, oldPresent := l.windows["old"]
	_, freshPresent := l.windows["fresh"]
	l.mu.Unlock()

	if oldPresent {
		t.Error("a stale window survived the sweep; the map grows once per token ever seen")
	}
	if !freshPresent {
		t.Error("the sweep discarded a live window")
	}
}

func TestForgetDropsARevokedTokensWindow(t *testing.T) {
	l := NewLimiter()
	l.Allow("t1", 60)
	l.Forget("t1")

	l.mu.Lock()
	_, present := l.windows["t1"]
	l.mu.Unlock()
	if present {
		t.Fatal("a revoked token kept its window")
	}
}

func TestBudgetRemainingNeverGoesNegative(t *testing.T) {
	for _, tc := range []struct {
		cap, spent, want float64
	}{
		{500, 0, 500},
		{500, 200, 300},
		{500, 500, 0},
		{500, 900, 0},
		{0, 0, 0},
	} {
		b := Budget{CapCents: tc.cap, SpentCents: tc.spent}
		if got := b.Remaining(); got != tc.want {
			t.Errorf("cap %.0f spent %.0f: remaining %.0f, want %.0f", tc.cap, tc.spent, got, tc.want)
		}
	}
}

// A rate of zero would refuse every request, which is revocation wearing a
// throttle's clothes and a confusing way to discover a token is dead.
func TestSetLimitsRefusesARateThatWouldRefuseEverything(t *testing.T) {
	s := &Store{}
	if err := s.SetLimits(nil, "u1", "t1", 0, 500); err == nil {
		t.Fatal("a rate of zero was accepted")
	}
	if err := s.SetLimits(nil, "u1", "t1", -1, 500); err == nil {
		t.Fatal("a negative rate was accepted")
	}
	if err := s.SetLimits(nil, "u1", "t1", 60, -1); err == nil {
		t.Fatal("a negative spend cap was accepted")
	}
}

// The defaults are the ones ADR-0025's successor beads assumed and that the
// migration writes. If one moves, the other has to.
func TestDefaultsMatchTheMigration(t *testing.T) {
	body := readMigration(t)
	for _, want := range []string{
		"rate_per_minute integer NOT NULL DEFAULT 60",
		"daily_spend_cents numeric(12,4) NOT NULL DEFAULT 500",
	} {
		if !contains(body, want) {
			t.Errorf("0041_api_token_limits.sql no longer contains %q", want)
		}
	}
}

func readMigration(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../db/migrations/0041_api_token_limits.sql")
	if err != nil {
		t.Fatalf("reading the migration: %v", err)
	}
	return string(b)
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
