package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/datopian/workgraph/internal/authn"
	"github.com/datopian/workgraph/internal/config"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testRoutes(auth authn.Authenticator) http.Handler {
	return routes(config.ControlAPI{Environment: config.EnvLocal}, nil, auth, quiet())
}

func TestHealthLiveIsOpen(t *testing.T) {
	rr := httptest.NewRecorder()
	testRoutes(&authn.StaticAuthenticator{}).ServeHTTP(rr, httptest.NewRequest("GET", "/health/live", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("liveness should be 200 without a token, got %d", rr.Code)
	}
}

// Readiness must reflect the database, not merely that the process is up. A
// load balancer sending traffic to an instance that cannot reach PostgreSQL is
// the failure this prevents.
func TestHealthReadyFailsWithoutDatabase(t *testing.T) {
	rr := httptest.NewRecorder()
	testRoutes(&authn.StaticAuthenticator{}).ServeHTTP(rr, httptest.NewRequest("GET", "/health/ready", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness with no database should be 503, got %d", rr.Code)
	}
}

// Every /v1 route is behind authentication, including ones not yet written.
// A new endpoint must not be reachable simply because nobody remembered to
// protect it.
func TestV1RequiresAuthentication(t *testing.T) {
	unauthenticated := &authn.StaticAuthenticator{} // empty identity -> denies
	for _, path := range []string{"/v1/me", "/v1/projects", "/v1/anything-future"} {
		rr := httptest.NewRecorder()
		testRoutes(unauthenticated).ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s returned %d; every /v1 route must require authentication", path, rr.Code)
		}
	}
}

func TestMeReturnsTheAuthenticatedIdentity(t *testing.T) {
	auth := &authn.StaticAuthenticator{Identity: authn.Identity{
		Subject: "sub-1", Email: "anu@datopian.com",
	}}
	rr := httptest.NewRecorder()
	testRoutes(auth).ServeHTTP(rr, httptest.NewRequest("GET", "/v1/me", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["email"] != "anu@datopian.com" {
		t.Errorf("unexpected identity: %v", body)
	}
}

// An unknown path outside /v1 is a 404, not a 501. A 501 on everything told a
// caller that any path they invented was a real but unimplemented endpoint.
func TestUnknownPathIsNotFound(t *testing.T) {
	rr := httptest.NewRecorder()
	testRoutes(&authn.StaticAuthenticator{}).ServeHTTP(rr, httptest.NewRequest("GET", "/nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}
