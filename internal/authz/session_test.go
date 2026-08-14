package authz

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// An empty user ID must never reach the database. It would set the session
// identity to the empty string, and a policy comparing against it could match
// rows belonging to nobody.
func TestWithUserRefusesEmptyUser(t *testing.T) {
	err := WithUser(context.Background(), nil, "", func(*sql.Tx) error {
		t.Fatal("callback ran with no user")
		return nil
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("expected a denial, got %v", err)
	}
}
