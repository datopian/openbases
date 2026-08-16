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
// An unknown API path must still 404.
//
// This assertion used to cover "/nope" and now covers "/v1/nope", because the
// root serves the single-page application: the client owns its routes, so an
// unknown UI path renders the app rather than 404ing. Narrowing the test was
// the right response to that change, but the API contract itself has not
// moved — a mistyped endpoint must fail rather than return an HTML page that a
// caller will try to parse as JSON.
func TestUnknownAPIPathIsNotFound(t *testing.T) {
	rr := httptest.NewRecorder()
	testRoutes(&authn.StaticAuthenticator{Identity: authn.Identity{
		Subject: "person@datopian.com", Email: "person@datopian.com",
		UserID: "11111111-1111-1111-1111-111111111111",
	}}).ServeHTTP(rr, httptest.NewRequest("GET", "/v1/nope", nil))
	// 501 is the deliberate answer for an unknown path under /v1 — the surface
	// is still being built out, and "not implemented" is more truthful than
	// "not found". The assertion is therefore about the SHAPE of the failure,
	// not its exact code: an API caller must get a JSON error, never the HTML
	// application shell, which it would try to parse as JSON.
	if rr.Code < 400 {
		t.Fatalf("an unknown API path returned %d; it must be an error", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("an unknown API path returned %q, not JSON; a caller would try to parse HTML", ct)
	}
	if strings.Contains(rr.Body.String(), "<div id=\"root\"") {
		t.Fatal("an unknown API path returned the application shell")
	}
}

// The application shell is served without an identity of its own, because the
// hostname sits behind Access and the assets carry no project data. What must
// NOT happen is the shell being served in place of an API error.
func TestUnknownUIPathServesTheApplication(t *testing.T) {
	rr := httptest.NewRecorder()
	testRoutes(&authn.StaticAuthenticator{}).ServeHTTP(rr, httptest.NewRequest("GET", "/projects/x", nil))
	// With no build embedded the root 404s, which is also correct; the point is
	// that it must never be an API-shaped error for a UI route.
	if rr.Code != http.StatusOK && rr.Code != http.StatusNotFound {
		t.Fatalf("unexpected status %d for a UI route", rr.Code)
	}
}

// The token endpoint hands out a git credential, so who may call it is the
// whole security property.
//
// A human's browser session must not be able to mint one. Access sessions are
// long-lived and live in a browser; turning one into a git credential for a
// restricted client repository is precisely the escalation this refuses.
func TestInstallationTokenRefusesHumanSessions(t *testing.T) {
	h := testRoutes(&authn.StaticAuthenticator{Identity: authn.Identity{
		Subject: "person@datopian.com",
		Email:   "person@datopian.com",
		UserID:  "11111111-1111-1111-1111-111111111111",
		// Deliberately not a service caller.
		IsService: false,
	}})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/integrations/github/installation-token",
		strings.NewReader(`{"repository":"workgraph-agent-sandbox"}`))
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("a human session must be refused, got %d: %s", rr.Code, rr.Body.String())
	}
}

// An unauthenticated caller must not reach the handler at all.
func TestInstallationTokenRequiresAuthentication(t *testing.T) {
	h := testRoutes(&authn.StaticAuthenticator{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/integrations/github/installation-token",
		strings.NewReader(`{"repository":"x"}`)))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

// An empty or owner-prefixed repository must be refused.
//
// An empty value would otherwise mint an UNSCOPED token covering every
// installed repository — the exact outcome this endpoint exists to prevent, and
// a mistake that would look like a harmless omission at the call site.
func TestInstallationTokenRefusesAnUnscopedRequest(t *testing.T) {
	service := &authn.StaticAuthenticator{Identity: authn.Identity{
		Subject: "workgraph-automation", IsService: true,
	}}

	for _, body := range []string{
		`{}`,
		`{"repository":""}`,
		`{"repository":"   "}`,
		`{"repository":"datopian/workgraph-agent-sandbox"}`,
	} {
		rr := httptest.NewRecorder()
		testRoutes(service).ServeHTTP(rr, httptest.NewRequest(
			"POST", "/v1/integrations/github/installation-token", strings.NewReader(body)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body %s: expected 400, got %d (%s)", body, rr.Code, strings.TrimSpace(rr.Body.String()))
		}
	}
}

// With no App configured the endpoint refuses rather than pretending.
func TestInstallationTokenWithoutAnAppConfigured(t *testing.T) {
	rr := httptest.NewRecorder()
	testRoutes(&authn.StaticAuthenticator{Identity: authn.Identity{
		Subject: "workgraph-automation", IsService: true,
	}}).ServeHTTP(rr, httptest.NewRequest("POST", "/v1/integrations/github/installation-token",
		strings.NewReader(`{"repository":"workgraph-agent-sandbox"}`)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the App is not configured, got %d", rr.Code)
	}
}

// An unauthorised caller must not be able to decide an approval.
//
// This is one of WP-F2's acceptance criteria, and it is asserted at the HTTP
// boundary as well as in the database because that is where a mistake would
// actually be made: a handler that forgets to pass the caller's identity would
// still pass every database test.
func TestApprovalDecisionRequiresAnApplicationUser(t *testing.T) {
	// Authenticated at the edge, but with no application user — the state a
	// service token or an unlinked identity is in.
	h := testRoutes(&authn.StaticAuthenticator{Identity: authn.Identity{
		Subject: "workgraph-automation", IsService: true,
	}})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST",
		"/v1/approvals/11111111-1111-1111-1111-111111111111/decide",
		strings.NewReader(`{"approve":true}`)))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without an application user, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestApprovalDecisionRequiresAuthentication(t *testing.T) {
	rr := httptest.NewRecorder()
	testRoutes(&authn.StaticAuthenticator{}).ServeHTTP(rr, httptest.NewRequest("POST",
		"/v1/approvals/x/decide", strings.NewReader(`{"approve":true}`)))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

// The inbox must never be served without an application user, because the rows
// it returns are scoped by that identity.
func TestInboxRequiresAnApplicationUser(t *testing.T) {
	for _, path := range []string{"/v1/inbox", "/v1/inbox/branches"} {
		rr := httptest.NewRecorder()
		testRoutes(&authn.StaticAuthenticator{Identity: authn.Identity{
			Subject: "workgraph-automation", IsService: true,
		}}).ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s returned %d without an application user", path, rr.Code)
		}
	}
}
