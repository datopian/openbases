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
	// Email is informational only. Empty for a service token.
	Email string
	// UserID is the internal application user this identity maps to.
	UserID string

	// IsService marks a machine caller authenticated by an Access service
	// token. A service token is not a person: it has no email, cannot be a
	// knowledge reviewer, and cannot satisfy an approval.
	IsService bool
	// ServiceName is the service token's common name, so an audit record
	// attributes the action to a named token rather than to nobody.
	ServiceName string

	// TokenID is set when the caller authenticated with a personal API token
	// rather than an interactive Access session, and holds that token's id.
	//
	// The identity is otherwise IDENTICAL to the session's — same UserID, same
	// grants, same row-level visibility — because that is exactly what makes a
	// token usable at all (wg-p4h.1). This field exists for the few decisions
	// that must still tell the two apart:
	//
	//	minting a token, which must never be reachable from a token, or a
	//	leaked credential mints its own successor and revocation stops
	//	meaning anything;
	//
	//	attributing an action in the audit trail to the credential used and
	//	not only to the person holding it.
	//
	// Empty means an interactive session. Code that cares must ask explicitly:
	// nothing else about a token identity is implicitly weaker, and treating it
	// as second-class by default would undo the property above.
	TokenID string
}

// ViaToken reports whether this identity came from a personal API token.
func (i Identity) ViaToken() bool { return i.TokenID != "" }

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
