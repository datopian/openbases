package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/datopian/workgraph/internal/authn"
	"github.com/datopian/workgraph/internal/config"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testRoutes(auth authn.Authenticator) http.Handler {
	return routes(config.ControlAPI{Environment: config.EnvLocal}, nil, auth, nil, quiet())
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

// The GitHub webhook is the ONE documented exception: GitHub cannot complete a
// Cloudflare Access challenge, so the endpoint authenticates itself with an
// HMAC signature instead.
//
// It must still refuse an unsigned request. Reaching the handler is not the
// same as being allowed in, and this test exists so the exception cannot
// quietly widen into an unauthenticated hole.
func TestWebhookBypassesAccessButStillAuthenticates(t *testing.T) {
	h := routes(
		config.ControlAPI{Environment: config.EnvLocal, GitHubWebhookSecret: "a-secret"},
		nil, &authn.StaticAuthenticator{}, nil, quiet(),
	)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/integrations/github/webhook", strings.NewReader(`{}`))
	h.ServeHTTP(rr, req)

	// 401 from signature verification, not from the Access middleware. Either
	// way it is refused; what matters is that an unsigned request never
	// succeeds.
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("an unsigned webhook must be refused, got %d: %s", rr.Code, rr.Body.String())
	}
}

// A correctly signed webhook is accepted without any Access token, which is the
// whole point of the exception.
func TestSignedWebhookIsAcceptedWithoutAccessToken(t *testing.T) {
	const secret = "a-secret"
	body := `{"action":"opened"}`

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	h := routes(
		config.ControlAPI{Environment: config.EnvLocal, GitHubWebhookSecret: secret},
		nil, &authn.StaticAuthenticator{}, nil, quiet(),
	)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/integrations/github/webhook", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", signature)
	req.Header.Set("X-GitHub-Delivery", "delivery-1")
	req.Header.Set("X-GitHub-Event", "pull_request")
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("a correctly signed webhook must be accepted, got %d: %s", rr.Code, rr.Body.String())
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
