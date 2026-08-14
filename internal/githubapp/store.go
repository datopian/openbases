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
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO github_deliveries (delivery_id, event_type, payload)
		VALUES ($1, $2, $3::jsonb)
		ON CONFLICT (delivery_id) DO NOTHING`,
		d.ID, d.Event, string(d.Body))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDuplicateDelivery
	}
	return nil
}
