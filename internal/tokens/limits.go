package tokens

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/datopian/openbases/internal/authz"
)

// Limiter enforces a per-token request rate.
//
// IN MEMORY, PER PROCESS, and that is a real limitation rather than an
// oversight, so it is stated here rather than discovered later:
//
//	it resets when the control API restarts, so a deploy grants every token a
//	fresh minute;
//
//	it counts per process, so if the API is ever run as more than one replica
//	the effective limit is the configured rate times the replica count.
//
// Both are acceptable today because there is exactly one control-api process
// and a restart is not a thing an attacker can cause. Neither stays acceptable
// if that changes, and the fix then is Cloudflare's edge rate limiting on
// api-staging.openbases.com — which is where volume should be stopped anyway,
// because traffic refused at the edge never reaches Go and costs nothing.
//
// What this DOES buy, and why it is worth having on its own: a runaway client
// with a valid token is throttled at the origin with a Retry-After the client
// can obey, and the refusal is attributed to a specific token so the operator
// knows which tool to fix.
type Limiter struct {
	mu      sync.Mutex
	windows map[string]*window
	// Now is overridable so tests do not sleep.
	Now func() time.Time
}

type window struct {
	start time.Time
	count int
}

// NewLimiter returns an empty Limiter.
func NewLimiter() *Limiter { return &Limiter{windows: map[string]*window{}} }

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Allow records a request against tokenID and reports whether it may proceed.
//
// The second return is how long to wait, and is only meaningful when the first
// is false. A fixed window rather than a sliding one: it is cheaper, and the
// failure mode — up to twice the rate across a window boundary — is far less
// bad than the alternative failure mode of an operator unable to predict when
// they may retry.
func (l *Limiter) Allow(tokenID string, perMinute int) (bool, time.Duration) {
	if tokenID == "" || perMinute <= 0 {
		return true, 0
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	w, ok := l.windows[tokenID]
	if !ok || now.Sub(w.start) >= time.Minute {
		l.windows[tokenID] = &window{start: now, count: 1}
		return true, 0
	}
	if w.count >= perMinute {
		return false, time.Minute - now.Sub(w.start)
	}
	w.count++
	return true, 0
}

// Forget drops a token's window. Called on revocation so a revoked token does
// not hold memory until the process restarts.
func (l *Limiter) Forget(tokenID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.windows, tokenID)
}

// Sweep discards windows older than a minute.
//
// Without it the map grows once per token ever seen, which is slow enough to
// take a long time to notice and is exactly the kind of leak that shows up as
// an unexplained restart six months later.
func (l *Limiter) Sweep() {
	cutoff := l.now().Add(-time.Minute)
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, w := range l.windows {
		if w.start.Before(cutoff) {
			delete(l.windows, id)
		}
	}
}

// Budget is a token's daily spend position.
type Budget struct {
	CapCents   float64
	SpentCents float64
	Exceeded   bool
}

// Remaining is what is left, floored at zero.
func (b Budget) Remaining() float64 {
	if b.SpentCents >= b.CapCents {
		return 0
	}
	return b.CapCents - b.SpentCents
}

// Limits returns a token's configured rate and its spend position today.
func (s *Store) Limits(ctx context.Context, tokenID string) (perMinute int, b Budget, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT t.rate_per_minute, b.cap_cents, b.spent_cents, b.exceeded
		   FROM api_tokens t, system_api_token_budget(t.id) b
		  WHERE t.id = $1`, tokenID,
	).Scan(&perMinute, &b.CapCents, &b.SpentCents, &b.Exceeded)
	switch {
	case err == sql.ErrNoRows:
		return 0, Budget{}, ErrNotFound
	case err != nil:
		return 0, Budget{}, fmt.Errorf("reading token limits: %w", err)
	}
	return perMinute, b, nil
}

// SetLimits changes a token's caps. Scoped to the caller's own tokens by
// row-level security, so a user id that does not own the token affects nothing.
func (s *Store) SetLimits(ctx context.Context, userID, tokenID string, perMinute int, capCents float64) error {
	if perMinute <= 0 {
		return fmt.Errorf("a rate of %d would refuse every request; use revocation to stop a token", perMinute)
	}
	if capCents < 0 {
		return fmt.Errorf("a negative spend cap is not a thing; zero means may not spend")
	}
	var affected int64
	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE api_tokens SET rate_per_minute = $1, daily_spend_cents = $2 WHERE id = $3`,
			perMinute, capCents, tokenID)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return fmt.Errorf("setting token limits: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}
