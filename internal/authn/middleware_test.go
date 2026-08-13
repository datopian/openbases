package authn

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type stubResolver struct {
	userID string
	err    error
}

func (s stubResolver) Resolve(_ context.Context, id Identity) (Identity, error) {
	if s.err != nil {
		return Identity{}, s.err
	}
	id.UserID = s.userID
	return id, nil
}

// The handler must never run for an unauthenticated request. A middleware that
// merely omits the identity would let a handler treat the caller as user "".
func TestMiddlewareNeverReachesHandlerUnauthenticated(t *testing.T) {
	reached := false
	h := Middleware(&AccessValidator{TeamDomain: "x", Audience: "y"}, nil, quietLogger())(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/me", nil))

	if reached {
		t.Fatal("handler ran for an unauthenticated request")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

// A caller with a valid token but no user record must be refused, which is what
// makes removing a user take effect immediately (WP-C2 acceptance).
func TestRemovedUserLosesAccessImmediately(t *testing.T) {
	reached := false
	auth := &StaticAuthenticator{Identity: Identity{Subject: "sub-1", Email: "gone@datopian.com"}}
	h := Middleware(auth, stubResolver{err: ErrNoSuchUser}, quietLogger())(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/me", nil))

	if reached {
		t.Fatal("a caller with no application user reached the handler")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestAuthenticatedRequestCarriesIdentity(t *testing.T) {
	auth := &StaticAuthenticator{Identity: Identity{Subject: "sub-1", Email: "anu@datopian.com"}}
	var got Identity
	var ok bool

	h := Middleware(auth, stubResolver{userID: "user-1"}, quietLogger())(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			got, ok = FromContext(r.Context())
		}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/me", nil))

	if !ok {
		t.Fatal("identity missing from the request context")
	}
	if got.UserID != "user-1" || got.Email != "anu@datopian.com" {
		t.Errorf("unexpected identity: %+v", got)
	}
}

// The failure reason must not leak: it tells an attacker which check to defeat.
func TestFailureReasonIsNotDisclosed(t *testing.T) {
	h := Middleware(&AccessValidator{TeamDomain: "x", Audience: "y"}, nil, quietLogger())(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/me", nil))

	body := rr.Body.String()
	for _, leak := range []string{"issuer", "audience", "signature", "expired", "certs"} {
		if contains(body, leak) {
			t.Errorf("response body discloses %q: %s", leak, body)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// FromContext must not hand back a usable identity when there is none.
func TestFromContextWithoutIdentity(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("an empty context must not yield an identity")
	}
}

var _ = errors.Is
