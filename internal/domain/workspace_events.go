package domain

import (
	"context"
	"database/sql"
	"time"

	"github.com/datopian/workgraph/internal/authz"
)

// WorkspaceEvent is one Google Workspace delivery, as a reader sees it.
//
// Metadata only. The raw Pub/Sub payload is deliberately not exposed: it is
// Google's message rather than ours, its shape is theirs to change, and
// returning it through the API would widen the surface for no benefit a reader
// has asked for. What a person wants to know is that something happened, to
// what, and when.
type WorkspaceEvent struct {
	Source      string     `json:"source"`
	Kind        string     `json:"kind"`
	EventType   string     `json:"event_type"`
	Target      string     `json:"target"`
	ReceivedAt  time.Time  `json:"received_at"`
	ProcessedAt *time.Time `json:"processed_at"`
}

// ProjectEventsBySlug returns the Workspace events belonging to one project.
//
// "Belonging" means the event's source is linked to the project. Today that is
// Meet spaces only: a shared drive serves many projects and the delivery names
// a file rather than the folder it sits in, so a Drive change cannot yet be
// attributed to one project without asking the Drive API for the file's
// parents. Those events exist and are visible through the company-wide view;
// they are simply not claimed by a project here.
//
// No visibility filtering happens in this function. Row-level security decides
// which rows come back, through the same can_read_source predicate the rest of
// the schema uses, so this query cannot forget a rule that lives elsewhere.
func (s *Store) ProjectEventsBySlug(ctx context.Context, userID, slug string, limit int) ([]WorkspaceEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	// Resolves the project first, so a caller who cannot see the project gets
	// ErrNotFound rather than an empty list. An empty list would answer "this
	// project has no events" to somebody who is not allowed to know the project
	// exists at all.
	summary, err := s.ProjectBySlug(ctx, userID, slug)
	if err != nil {
		return nil, err
	}

	out := []WorkspaceEvent{}
	err = authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT s.display_name, s.kind, COALESCE(r.event_type, ''),
			       COALESCE(r.target, ''), r.received_at, r.processed_at
			  FROM event_receipts r
			  JOIN event_sources s ON s.id = r.source_id
			 WHERE s.project_id = $1::uuid
			 ORDER BY r.received_at DESC
			 LIMIT $2`, summary.ID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e WorkspaceEvent
			var processed sql.NullTime
			if err := rows.Scan(&e.Source, &e.Kind, &e.EventType, &e.Target,
				&e.ReceivedAt, &processed); err != nil {
				return err
			}
			if processed.Valid {
				e.ProcessedAt = &processed.Time
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}
