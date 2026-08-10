// Package authn validates the caller's identity.
//
// Cloudflare Access is the external authentication layer, but the application
// validates the Access JWT itself rather than trusting request headers. A
// forged header without a valid signed token must fail (plan section 8.1,
// WP-C2 acceptance).
package authn

import (
	"context"
	"errors"
	"net/http"
)

// ErrUnauthenticated is returned when no valid identity can be established.
var ErrUnauthenticated = errors.New("unauthenticated")

// ErrNotImplemented marks a capability that WP-C2 has not yet delivered.
// Returning it keeps the service fail-closed: no caller is authenticated by
// default, so an unfinished implementation cannot become an open door.
var ErrNotImplemented = errors.New("authenticator not implemented (WP-C2)")

// Identity is an authenticated principal. The immutable provider subject is the
// key; email is an attribute and is never the primary database key
// (plan section 8.1).
type Identity struct {
	// Subject is the immutable identifier from the identity provider.
	Subject string
	// Email is informational only.
	Email string
	// UserID is the internal application user this identity maps to.
	UserID string
}

// Authenticator establishes the identity of an inbound request.
type Authenticator interface {
	// Authenticate returns the caller identity, or an error. It must never
	// return a zero Identity with a nil error.
	Authenticate(ctx context.Context, r *http.Request) (Identity, error)
}

// AccessAuthenticator validates a Cloudflare Access JWT against the team's
// public keys.
type AccessAuthenticator struct {
	TeamDomain string
	Audience   string
}

// Authenticate is not yet implemented. It deliberately denies every request so
// that deploying an incomplete build cannot expose the application.
func (a *AccessAuthenticator) Authenticate(_ context.Context, _ *http.Request) (Identity, error) {
	return Identity{}, ErrNotImplemented
}

// StaticAuthenticator returns a fixed identity. It exists only for local
// development and tests and must never be constructed outside them.
type StaticAuthenticator struct{ Identity Identity }

// Authenticate returns the configured identity.
func (s *StaticAuthenticator) Authenticate(_ context.Context, _ *http.Request) (Identity, error) {
	if s.Identity.Subject == "" {
		return Identity{}, ErrUnauthenticated
	}
	return s.Identity, nil
}
