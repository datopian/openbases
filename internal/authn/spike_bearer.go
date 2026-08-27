//go:build spike

// SPIKE CODE — wg-p4h.1. Not a product feature and not built by default.
//
// The build tag is deliberate. This file exists to answer one question before
// wg-p4h.2 commits to a design: can a credential that is NOT a Cloudflare
// Access JWT carry an application user through the stack so that the row-level
// security policies behave exactly as they do for a browser session?
//
// There is no issuance, no storage, no hashing, no scopes and no expiry here.
// wg-p4h.2 builds those. Anything in this file that looks like a shortcut is
// one, and it is why the tag keeps it out of a production binary.

package authn

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// errNotMine signals that an authenticator did not RECOGNISE the credential, as
// distinct from recognising it and refusing.
//
// The distinction is load-bearing in a chain. If "no bearer header" and "bad
// bearer token" are the same error, a malformed token falls through to the next
// authenticator and the caller gets whichever refusal the LAST link produces —
// so a mistyped token reports "no Access JWT", which sends the operator to
// debug the wrong credential entirely.
var errNotMine = errors.New("credential not recognised by this authenticator")

// BearerAuthenticator recognises `Authorization: Bearer <token>`.
//
// SPIKE: one hard-coded token and one hard-coded user. The real thing looks this
// token up in api_tokens by hash and reads the user from the row.
type BearerAuthenticator struct {
	// TokenSHA256 is the digest of the single accepted token. Even in a spike
	// the plaintext is not held in memory: the comparison the real
	// implementation performs is against a stored digest, and a spike that
	// compares plaintext would prove the easy version of the problem.
	TokenSHA256 [32]byte
	// Subject and UserID are what the token resolves to.
	Subject string
	UserID  string
}

// Authenticate returns the token's owner.
//
// The identity returned sets IsService FALSE. That is the whole point: a
// personal token is a credential for a person, so it must take the resolver's
// user branch rather than the machine branch that leaves UserID empty.
func (b *BearerAuthenticator) Authenticate(_ context.Context, r *http.Request) (Identity, error) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return Identity{}, errNotMine
	}
	token, ok := bearerToken(raw)
	if !ok {
		return Identity{}, errNotMine
	}
	if token == "" {
		return Identity{}, fmt.Errorf("%w: empty bearer token", ErrUnauthenticated)
	}

	// Constant-time, for the same reason internal/githubapp compares the
	// webhook signature that way: a length-dependent or early-exit comparison
	// on a secret leaks it a byte at a time.
	got := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(got[:], b.TokenSHA256[:]) != 1 {
		return Identity{}, fmt.Errorf("%w: bearer token not recognised", ErrUnauthenticated)
	}

	return Identity{Subject: b.Subject, UserID: b.UserID, IsService: false}, nil
}

// bearerToken extracts the credential from an Authorization header. The scheme
// is case-insensitive per RFC 7235.
func bearerToken(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	return strings.TrimSpace(header[len(scheme):]), true
}

// Chain tries each authenticator in order.
//
// It stops at the first that RECOGNISES the credential, whether it then accepts
// or refuses it, and falls through only on errNotMine. Reaching the end with
// nothing recognised is ErrUnauthenticated, so the chain is fail-closed at both
// ends: an empty chain authenticates nobody.
type Chain []Authenticator

// Authenticate implements Authenticator.
func (c Chain) Authenticate(ctx context.Context, r *http.Request) (Identity, error) {
	for _, a := range c {
		id, err := a.Authenticate(ctx, r)
		switch {
		case err == nil:
			// An authenticator that returns a zero Identity with a nil error
			// would authenticate a request as user "". The interface forbids
			// it; the chain refuses to rely on that being honoured.
			if id.Subject == "" {
				return Identity{}, fmt.Errorf("%w: authenticator returned no subject", ErrUnauthenticated)
			}
			return id, nil
		case errors.Is(err, errNotMine):
			continue
		default:
			return Identity{}, err
		}
	}
	return Identity{}, ErrUnauthenticated
}
