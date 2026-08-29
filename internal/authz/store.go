package authz

import (
	"context"
	"database/sql"
	"fmt"
)

// Store is the PostgreSQL Authorizer.
//
// The interface has existed since the package was written, with a comment
// promising that "the production implementation reads scoped role grants from
// PostgreSQL". It did not exist, and could not: the matrix connecting roles to
// actions was never written down. ADR-0026 decided it, 0040_role_permissions.sql
// stores it, and this reads it.
//
// Deny-by-default, identical to GrantSet: an unrecognised action is denied
// rather than treated as unconstrained, a missing user is denied, and an error
// is denied rather than passed through as permission.
type Store struct{ db *sql.DB }

// NewStore returns an Authorizer over db.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Authorize reports whether the user may perform the action.
//
// It runs WITHOUT a user session, through a SECURITY DEFINER function, and that
// is deliberate rather than convenient: this decides whether to admit a request,
// so it cannot depend on the row-level security context that the request has not
// established yet. The function it calls reads only role_grants and
// role_permissions and returns a boolean — nothing an attacker could mine.
func (s *Store) Authorize(ctx context.Context, req Request) error {
	if req.UserID == "" {
		return ErrDenied
	}
	if !req.Action.Known() {
		// The set is closed. A typo is a denial, not a new permission.
		return ErrDenied
	}
	if s == nil || s.db == nil {
		// No database is not "allow". A build wired without one must refuse
		// rather than serve as though everyone were permitted.
		return fmt.Errorf("%w: no authorisation store", ErrDenied)
	}

	var project any
	if req.ProjectID != "" {
		project = req.ProjectID
	}

	var ok bool
	err := s.db.QueryRowContext(ctx,
		`SELECT system_user_may($1, $2, $3)`, req.UserID, string(req.Action), project,
	).Scan(&ok)
	if err != nil {
		// An outage denies. Reporting it as permission would be the one failure
		// mode this package exists to prevent.
		return fmt.Errorf("%w: authorisation could not be determined: %v", ErrDenied, err)
	}
	if !ok {
		return ErrDenied
	}
	return nil
}

var _ Authorizer = (*Store)(nil)
