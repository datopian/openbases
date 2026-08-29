// Package idempotency makes a write endpoint safe for a caller that retries.
//
// Plan §12.3 requires idempotency keys on write and action endpoints, and no
// endpoint had one. That was survivable while the only writer was a person
// clicking once. An agent retries on a timeout it cannot distinguish from a
// failure, and "dispatch this bead" executed twice spends twice (wg-p4h.4).
//
// Three outcomes, and the third is the one people get wrong:
//
//	no key seen before      the handler runs, and its response is recorded
//	same key, same request  the recorded response is replayed; the handler does not run
//	same key, DIFFERENT request  409, and the handler does not run
//
// The last is a conflict rather than a replay because the caller believes they
// are sending something new. Returning the old response would give them the
// wrong answer and hide the bug that produced two different bodies under one
// key.
package idempotency

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/datopian/workgraph/internal/authz"
)

// Header is the request header carrying the key.
const Header = "Idempotency-Key"

// MaxKeyLength bounds what will be stored, matching the CHECK in
// 0042_idempotency.sql.
const MaxKeyLength = 255

// ErrConflict reports the same key used for a different request.
var ErrConflict = errors.New("this idempotency key was used for a different request")

// Record is a replayable response.
type Record struct {
	StatusCode int
	Body       json.RawMessage
}

// Store persists keys and their responses.
type Store struct{ db *sql.DB }

// NewStore returns a Store over db.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Digest is the hash of a request body, used to tell a replay from a conflict.
func Digest(body []byte) [32]byte { return sha256.Sum256(body) }

// Lookup returns a recorded response for this key, if one exists.
//
// Returns (nil, nil) when the key is new — the caller should run the handler.
// Returns ErrConflict when the key exists against a different request.
func (s *Store) Lookup(ctx context.Context, userID, key, route string, digest [32]byte) (*Record, error) {
	if key == "" {
		return nil, nil
	}
	var (
		rec       Record
		storedSHA []byte
		storedRt  string
	)
	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT status_code, response, request_sha, route
			   FROM idempotency_keys WHERE key = $1`, key,
		).Scan(&rec.StatusCode, &rec.Body, &storedSHA, &storedRt)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("looking up an idempotency key: %w", err)
	}

	// A key replayed against a different route would return one endpoint's
	// response to another endpoint's call, which is worse than a plain conflict
	// because the shape would look plausible.
	if storedRt != route {
		return nil, fmt.Errorf("%w: first used on %s", ErrConflict, storedRt)
	}
	if !equalDigest(storedSHA, digest) {
		return nil, ErrConflict
	}
	return &rec, nil
}

// Record stores a response against a key.
//
// A duplicate insert is not an error: two concurrent requests with the same key
// race, one wins, and the loser's caller gets an equivalent answer. Failing the
// second would turn a successful write into a reported failure and invite a
// third attempt.
func (s *Store) Record(ctx context.Context, userID, key, route string, digest [32]byte, status int, body []byte) error {
	if key == "" {
		return nil
	}
	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_keys (user_id, key, route, request_sha, status_code, response)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (user_id, key) DO NOTHING`,
			userID, key, route, digest[:], status, body)
		return err
	})
	if err != nil {
		return fmt.Errorf("recording an idempotency key: %w", err)
	}
	return nil
}

// Sweep discards keys past the retry horizon.
func (s *Store) Sweep(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT system_sweep_idempotency_keys()`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sweeping idempotency keys: %w", err)
	}
	return n, nil
}

// ValidKey reports whether a key is usable. An over-long key is refused rather
// than truncated: truncating would make two different keys collide, which is
// the one thing this mechanism must never do.
func ValidKey(key string) bool {
	return key != "" && len(key) <= MaxKeyLength
}

func equalDigest(stored []byte, want [32]byte) bool {
	if len(stored) != len(want) {
		return false
	}
	for i := range want {
		if stored[i] != want[i] {
			return false
		}
	}
	return true
}
