package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Store is the reconciler's view of the database.
//
// Every call goes through a system_* function rather than a table, because all
// three tables are behind FORCE ROW LEVEL SECURITY and the reconciler runs on a
// timer with no user. Reading a table directly would return nothing, and
// nothing means "no allow-listed sources", which Reconcile would act on by
// deleting every subscription it found.
type DB struct{ Conn *sql.DB }

// Load reads the allow-list and what we believe about each subscription.
//
// Both in one call. Fetching them separately invites a race where a source
// added between the two queries appears as a subscription with no source, which
// Reconcile would decide to delete.
func (s *DB) Load(ctx context.Context) ([]Source, []Subscription, error) {
	rows, err := s.Conn.QueryContext(ctx, `SELECT * FROM system_event_sources()`)
	if err != nil {
		return nil, nil, fmt.Errorf("loading event sources: %w", err)
	}
	defer rows.Close()

	var sources []Source
	var subs []Subscription
	for rows.Next() {
		var (
			id, kind, ext, name, vis string
			enabled                  bool
			googleName               sql.NullString
			state                    string
			expires                  sql.NullTime
			types                    []byte
		)
		if err := rows.Scan(&id, &kind, &ext, &name, &vis, &enabled,
			&googleName, &state, &expires, &types); err != nil {
			return nil, nil, err
		}
		sources = append(sources, Source{
			ID: id, Kind: Kind(kind), ExternalID: ext, Name: name,
			Visibility: Visibility(vis), Enabled: enabled,
		})
		// A row with no google_name is a source we have never subscribed for.
		// Recorded as a Subscription only when one exists, so Reconcile sees
		// "no subscription" rather than an empty one that looks broken.
		if googleName.Valid && googleName.String != "" {
			var eventTypes []string
			if len(types) > 0 {
				if err := json.Unmarshal(types, &eventTypes); err != nil {
					return nil, nil, fmt.Errorf("reading the event types of %s: %w", name, err)
				}
			}
			sub := Subscription{
				SourceID: id, GoogleName: googleName.String,
				State: State(state), EventTypes: eventTypes,
			}
			if expires.Valid {
				sub.ExpiresAt = expires.Time
			}
			subs = append(subs, sub)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// No second query for orphans: source_id is ON DELETE CASCADE, so a
	// subscription row cannot outlive its source. The orphan that can exist is
	// one live in Google with no row here, and that one is found by asking
	// Google — see Reconciler.adopt.
	return sources, subs, nil
}

// Record saves what Google now holds for a source.
func (s *DB) Record(ctx context.Context, sourceID string, g GoogleSubscription, failure error) error {
	var expires any
	if t, ok := g.Expiry(); ok {
		expires = t
	}
	var errText any
	state := g.LifecycleState()
	if failure != nil {
		errText = failure.Error()
		// A failed attempt does not get to claim a state. Recording 'failed'
		// rather than whatever Google last said keeps the next pass from
		// treating a broken subscription as active.
		state = StateFailed
	}
	types, err := json.Marshal(g.EventTypes)
	if err != nil {
		return err
	}
	var ok bool
	return s.Conn.QueryRowContext(ctx,
		`SELECT system_record_subscription($1::uuid, $2, $3, $4::timestamptz, $5::jsonb, $6)`,
		sourceID, nullableStr(g.Name), string(state), expires, string(types), errText,
	).Scan(&ok)
}

// Forget marks a subscription gone.
func (s *DB) Forget(ctx context.Context, sourceID string) error {
	var ok bool
	return s.Conn.QueryRowContext(ctx,
		`SELECT system_forget_subscription($1::uuid)`, sourceID).Scan(&ok)
}

// Summary is what arrived, per source and event type.
type Summary struct {
	Source      string
	Kind        string
	EventType   string
	Deliveries  int64
	Latest      time.Time
	Unprocessed int64
}

// Summarise reports deliveries since a time, or all of them when zero.
func (s *DB) Summarise(ctx context.Context, since time.Time) ([]Summary, error) {
	var arg any
	if !since.IsZero() {
		arg = since
	}
	rows, err := s.Conn.QueryContext(ctx, `SELECT * FROM system_event_summary($1::timestamptz)`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var r Summary
		var latest sql.NullTime
		if err := rows.Scan(&r.Source, &r.Kind, &r.EventType, &r.Deliveries, &latest, &r.Unprocessed); err != nil {
			return nil, err
		}
		if latest.Valid {
			r.Latest = latest.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
