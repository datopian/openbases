package githubapp

import (
	"context"
	"database/sql"
	"errors"
)

// ErrDuplicateDelivery reports a delivery already recorded.
//
// GitHub retries on any non-2xx, and the same delivery ID arrives again. The
// receipt is stored before processing so a retry is recognised rather than
// applied twice (plan section 9.3).
var ErrDuplicateDelivery = errors.New("delivery already received")

// Store records webhook receipts.
type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// RecordDelivery stores an immutable receipt and reports whether it is new.
//
// The receipt is written before any processing, so an event whose handler
// crashes is still known to have arrived — the alternative is silently
// reprocessing it on every retry.
func (s *Store) RecordDelivery(ctx context.Context, d *Delivery) error {
	// Through the system function: github_deliveries is protected because the
	// payloads carry titles and branch names from every installed repository,
	// restricted ones included, and a webhook has no user identity to check.
	var recorded bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT system_record_delivery($1, $2, $3::jsonb)`,
		d.ID, d.Event, string(d.Body)).Scan(&recorded); err != nil {
		return err
	}
	if !recorded {
		return ErrDuplicateDelivery
	}
	return nil
}

// MarkProcessed records that a delivery's projection completed.
//
// Deliveries with a receipt but no processed_at are exactly the ones to replay
// after a crash: the event was accepted and is durable, but its effect on the
// domain model may never have landed.
func (s *Store) MarkProcessed(ctx context.Context, deliveryID string) error {
	_, err := s.db.ExecContext(ctx, `SELECT system_mark_delivery_processed($1)`, deliveryID)
	return err
}
