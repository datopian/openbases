package authn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// TokenLookup resolves a presented secret to its owner.
//
// An interface rather than a direct dependency on internal/tokens, so this
// package keeps no database import and the chain stays testable without one.
type TokenLookup interface {
	// Authenticate returns the token's owner, or an error. The error must not
	// distinguish "no such token" from revoked, expired, or a suspended owner:
	// telling them apart lets a caller learn whether a token ever existed.
	Authenticate(ctx context.Context, secret string) (tokenID, userID string, scopes []string, err error)
	// RecordUse notes that a token was used. Best-effort by contract.
	RecordUse(ctx context.Context, tokenID string) error
}

// BearerAuthenticator authenticates `Authorization: Bearer <token>`.
//
// The identity it produces is deliberately INDISTINGUISHABLE from an Access
// session's except for TokenID: same UserID, so authz.WithUser sets the same
// workgraph.user_id and every row-level security policy decides identically.
// wg-p4h.1 proved that is structural — provenance is not an input a policy has
// — and this is the code that relies on it.
//
// IsService stays false. A personal token is a person's credential, so it must
// take the resolver's user branch rather than the machine branch that leaves
// UserID empty.
type BearerAuthenticator struct {
	Lookup TokenLookup
	Log    *slog.Logger
}

// Authenticate implements Authenticator.
func (b *BearerAuthenticator) Authenticate(ctx context.Context, r *http.Request) (Identity, error) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return Identity{}, ErrNotRecognised
	}
	secret, ok := bearerToken(raw)
	if !ok {
		// Some other scheme. Not ours to refuse.
		return Identity{}, ErrNotRecognised
	}
	if secret == "" {
		return Identity{}, fmt.Errorf("%w: empty bearer token", ErrUnauthenticated)
	}
	if b == nil || b.Lookup == nil {
		// Configured without a store. Refusing rather than falling through:
		// falling through would make a misconfiguration look like a missing
		// credential, and the operator would go and check their token.
		return Identity{}, fmt.Errorf("%w: token authentication is not configured", ErrUnauthenticated)
	}

	tokenID, userID, scopes, err := b.Lookup.Authenticate(ctx, secret)
	if err != nil {
		// Recognised and refused, so the chain stops here. The reason is never
		// returned to the caller.
		return Identity{}, fmt.Errorf("%w: bearer token not accepted", ErrUnauthenticated)
	}

	// Best-effort, and deliberately not fatal. Losing a timestamp is a
	// nuisance; refusing a valid request because a bookkeeping write failed is
	// an outage.
	if err := b.Lookup.RecordUse(ctx, tokenID); err != nil && b.Log != nil {
		b.Log.Warn("recording token use", "token", tokenID, "error", err)
	}

	return Identity{
		Subject: "token:" + tokenID,
		UserID:  userID,
		TokenID: tokenID,
		Scopes:  scopes,
	}, nil
}

// bearerToken extracts the credential. The scheme is case-insensitive per
// RFC 7235.
func bearerToken(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	return strings.TrimSpace(header[len(scheme):]), true
}

// AccessOrNotRecognised adapts the Access validator to the chain.
//
// The validator refuses a request with no Access token the same way it refuses
// a bad one, which is correct on its own but wrong inside a chain: with the
// token link after it, "no JWT present" must fall through rather than end the
// request. This translates exactly that one case and passes everything else
// along unchanged.
type AccessOrNotRecognised struct{ Inner Authenticator }

// Authenticate implements Authenticator.
func (a AccessOrNotRecognised) Authenticate(ctx context.Context, r *http.Request) (Identity, error) {
	if !hasAccessToken(r) {
		return Identity{}, ErrNotRecognised
	}
	return a.Inner.Authenticate(ctx, r)
}

// hasAccessToken reports whether the request carries an Access assertion at
// all, in either of the two places Cloudflare puts one.
func hasAccessToken(r *http.Request) bool {
	if r.Header.Get("Cf-Access-Jwt-Assertion") != "" {
		return true
	}
	if c, err := r.Cookie("CF_Authorization"); err == nil && c.Value != "" {
		return true
	}
	return false
}

var (
	_ Authenticator = (*BearerAuthenticator)(nil)
	_ Authenticator = AccessOrNotRecognised{}
	_               = errors.Is
)
