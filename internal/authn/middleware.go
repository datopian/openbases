package authn

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

type contextKey struct{ name string }

var identityKey = &contextKey{"workgraph.identity"}

// FromContext returns the authenticated identity, or false when the request was
// not authenticated. A handler that ignores the second value would treat an
// anonymous request as user "", which is why it is not optional.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey).(Identity)
	return id, ok
}

// NewContext attaches an identity. Exported for tests and for the resolver that
// maps a provider subject to an application user.
func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// Resolver maps an authenticated provider identity to an application user.
//
// Authentication says who the caller is at the edge; this says whether they are
// a Workgraph user at all. A valid Access token for someone with no user record
// must not become a session (plan section 8.1).
type Resolver interface {
	Resolve(ctx context.Context, id Identity) (Identity, error)
}

// ErrNoSuchUser is returned when a validly authenticated caller has no
// application user. Removing someone's user record therefore ends their access
// immediately, without waiting for a token to expire.
var ErrNoSuchUser = errors.New("no application user for this identity")

// Middleware authenticates every request and attaches the identity.
//
// It fails closed: any error is a 401, and the handler never runs. There is no
// path through this function that reaches the next handler without an identity.
func Middleware(a Authenticator, r Resolver, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id, err := a.Authenticate(req.Context(), req)
			if err != nil {
				// The reason is logged, never returned. Telling a caller which
				// check failed helps them craft a token that passes it.
				log.Warn("authentication failed",
					"path", req.URL.Path,
					"remote", req.Header.Get("Cf-Connecting-Ip"),
					"error", err)
				unauthorized(w)
				return
			}

			if r != nil {
				resolved, err := r.Resolve(req.Context(), id)
				if err != nil {
					log.Warn("identity has no application user",
						"subject", id.Subject, "service", id.IsService, "error", err)
					unauthorized(w)
					return
				}
				id = resolved
			}

			next.ServeHTTP(w, req.WithContext(NewContext(req.Context(), id)))
		})
	}
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
}
