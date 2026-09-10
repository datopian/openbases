package main

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// waitForJob blocks until a queued job reaches a terminal state, or the
// deadline passes.
//
// Filing has to travel to the node — a bead lives in a Dolt graph on the
// execution host — so the honest options were "return a job id and let the
// caller poll" or "wait a little". Polling was tried and reported as a bug:
// a filing that returns {status: queued} and nothing else leaves the caller
// unable to tell working from dead, and the recovery was to re-submit and
// hope.
//
// So this waits, and gives up cleanly. Giving up is not failure: the job is
// queued and will run, and the caller gets its id.
//
// Polled rather than notified because that is what the rest of the system
// does. A LISTEN/NOTIFY channel would be faster and is a second mechanism to
// keep working; the node itself only looks every ten seconds, so the ceiling
// on how fast this could possibly answer is set there, not here.
func waitForJob(ctx context.Context, db *sql.DB, jobID string, limit time.Duration) (status, result string) {
	// Half a second: fast enough that a filing which lands quickly answers
	// quickly, slow enough that a 45-second wait is 90 queries and not 45,000.
	const every = 500 * time.Millisecond

	deadline := time.Now().Add(limit)
	for {
		var s, r sql.NullString
		err := db.QueryRowContext(ctx,
			`SELECT status, coalesce(result, '') FROM work_queue WHERE id = $1`, jobID).
			Scan(&s, &r)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// It was enqueued a moment ago, so this means the row is not
			// visible yet rather than gone. Keep waiting.
		case err != nil:
			// A database that cannot answer is not a reason to fail the
			// filing: the job exists and the caller has its id.
			return "queued", ""
		case s.String == "done" || s.String == "failed":
			return s.String, r.String
		}

		if time.Now().After(deadline) {
			if s.Valid && s.String != "" {
				return s.String, r.String
			}
			return "queued", ""
		}
		select {
		case <-ctx.Done():
			// The caller hung up. Nothing to report to.
			return "queued", ""
		case <-time.After(every):
		}
	}
}
