package authz

import (
	"context"
	"database/sql"
	"fmt"
)

// WithUser runs fn on a connection whose session carries the caller's user ID,
// which is what the row-level security policies read.
//
// This is the join between application authorisation and the database backstop.
// 0008_rls.sql defines current_app_user() as a session setting; without this,
// that function returns NULL and every policy denies — so a missing call fails
// closed rather than exposing data. That is the correct direction, but it also
// means every read path must go through here.
//
// set_config's third argument is true, making the setting local to the
// transaction. A pooled connection therefore cannot leak one caller's identity
// into the next caller's query.
func WithUser(ctx context.Context, db *sql.DB, userID string, fn func(*sql.Tx) error) error {
	if userID == "" {
		return fmt.Errorf("refusing to open a session with no user: %w", ErrDenied)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`SELECT set_config('workgraph.user_id', $1, true)`, userID); err != nil {
		return fmt.Errorf("setting session identity: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
