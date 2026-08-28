package authn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// ErrNotRecognised signals that an authenticator did not RECOGNISE a
// credential, as distinct from recognising it and refusing it.
//
// The distinction is load-bearing rather than tidy, and wg-p4h.1's spike found
// it the hard way. If a chain treats the two alike, a mistyped bearer token
// falls through to the Access link and the caller receives ITS refusal — so
// somebody with a typo in their token is told their Access JWT is missing, and
// spends the afternoon debugging the wrong credential.
//
// An authenticator returns this when the request carries nothing it knows how
// to read. Anything else it returns stops the chain.
var ErrNotRecognised = errors.New("credential not recognised by this authenticator")

// Chain tries each authenticator in order and stops at the first that
// recognises the credential, whether it then accepts or refuses it.
//
// Fail-closed at both ends: an empty chain authenticates nobody, and reaching
// the end with nothing recognised is ErrUnauthenticated.
type Chain []Authenticator

// Authenticate implements Authenticator.
func (c Chain) Authenticate(ctx context.Context, r *http.Request) (Identity, error) {
	for _, a := range c {
		id, err := a.Authenticate(ctx, r)
		switch {
		case err == nil:
			// The interface says an authenticator must never return a zero
			// Identity with a nil error. The chain refuses to depend on every
			// implementation having honoured that, because the failure mode is
			// authenticating a request as user "".
			if id.Subject == "" {
				return Identity{}, fmt.Errorf("%w: authenticator returned no subject", ErrUnauthenticated)
			}
			return id, nil
		case errors.Is(err, ErrNotRecognised):
			continue
		default:
			// Recognised and refused. Later links do not get a turn: a revoked
			// Access session must not be rescued by a token in the same
			// request, and vice versa.
			return Identity{}, err
		}
	}
	return Identity{}, ErrUnauthenticated
}
