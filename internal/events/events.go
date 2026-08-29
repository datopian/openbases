// Package events reads the append-only domain event log for clients that would
// otherwise poll (wg-p4h.8).
//
// Plan §12.3 lists GET /v1/events/stream and the API did not have it. §12.2
// already defines the event model, so this is a read over a substrate that
// exists rather than new machinery.
//
// Without it, an agent that wants to know when a dispatch finished polls
// /v1/work every few seconds, per agent, forever. That is a cost with no
// ceiling, and it arrives as database load on the control node rather than as a
// bill anybody notices.
//
// Visibility is not re-implemented here. events already carries RLS keyed on
// project (0010_rls_coverage.sql), so every read goes through authz.WithUser and
// the policy decides — which is the only way a stream cannot drift from the
// table it streams. A stream that leaks a project the caller cannot see is worse
// than no stream, because it leaks continuously rather than once.
package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/datopian/workgraph/internal/authz"
)

// Event is one entry, in the shape the API returns.
type Event struct {
	ID         int64           `json:"id"`
	OccurredAt time.Time       `json:"occurred_at"`
	Type       string          `json:"type"`
	Project    *string         `json:"project,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

// Page is a cursor-paginated read.
//
// The cursor is the id of the last event returned, not an offset. An offset
// shifts under inserts, so a client paging an append-only log with OFFSET
// silently skips rows exactly when the log is busiest — which is when somebody
// is most likely to be reading it.
type Page struct {
	Events []Event `json:"events"`
	// NextCursor is empty when the caller has reached the end.
	NextCursor string `json:"next_cursor,omitempty"`
}

// Store reads events.
type Store struct{ db *sql.DB }

// NewStore returns a Store over db.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// DefaultLimit is the page size when a caller does not ask.
const DefaultLimit = 100

// MaxLimit caps what a caller may ask for. A client that requests everything
// gets a page, not a copy of the log.
const MaxLimit = 500

// After returns events with an id greater than the cursor, oldest first.
//
// Oldest first, deliberately. A client resuming a stream needs to apply what it
// missed in the order it happened; newest-first would make it replay history
// backwards and reach the wrong state.
func (s *Store) After(ctx context.Context, userID string, afterID int64, limit int) (Page, error) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	var page Page
	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT e.id, e.occurred_at, e.type, p.slug, e.payload
			   FROM events e
			   LEFT JOIN projects p ON p.id = e.project_id
			  WHERE e.id > $1
			  ORDER BY e.id ASC
			  LIMIT $2`, afterID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev Event
			var slug sql.NullString
			var payload []byte
			if err := rows.Scan(&ev.ID, &ev.OccurredAt, &ev.Type, &slug, &payload); err != nil {
				return err
			}
			if slug.Valid {
				v := slug.String
				ev.Project = &v
			}
			ev.Payload = payload
			page.Events = append(page.Events, ev)
		}
		return rows.Err()
	})
	if err != nil {
		return Page{}, fmt.Errorf("reading events: %w", err)
	}

	// A cursor is returned only when the page was full. Returning one on a
	// short page would tell a client there is more when there is not, and it
	// would poll for it — the behaviour this endpoint exists to remove.
	if len(page.Events) == limit && limit > 0 {
		page.NextCursor = fmt.Sprint(page.Events[len(page.Events)-1].ID)
	}
	return page, nil
}

// Latest returns the id of the newest event the caller may see, so a stream can
// start from "now" without replaying history.
func (s *Store) Latest(ctx context.Context, userID string) (int64, error) {
	var id sql.NullInt64
	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT max(id) FROM events`).Scan(&id)
	})
	if err != nil {
		return 0, fmt.Errorf("reading the latest event: %w", err)
	}
	return id.Int64, nil
}
