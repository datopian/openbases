package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/datopian/workgraph/internal/authn"
	"github.com/datopian/workgraph/internal/config"
	wgmcp "github.com/datopian/workgraph/internal/mcp"
)

// /mcp is behind Access, is not a bypass, and refuses a caller with no
// identity (wg-p4h.11).

const appBase = "https://work-staging.openbases.com"

func mcpCfg() config.ControlAPI {
	return config.ControlAPI{Environment: config.EnvLocal, AppBaseURL: appBase}
}

// The route is authenticated. Access refuses an anonymous request before it
// reaches the origin, and this is the second lock: if Access is ever
// misconfigured, or somebody reaches the node directly, /mcp still says no.
func TestMCPRefusesACallerWithNoIdentity(t *testing.T) {
	h := routes(mcpCfg(), nil, &authn.StaticAuthenticator{}, nil, quiet())
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(method, "/mcp", strings.NewReader(`{}`)))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s /mcp without an identity returned %d, want 401: %s",
				method, rr.Code, rr.Body.String())
		}
	}
}

// The bypass list is two routes and /mcp is not one of them. Asserted here as
// well as in TestOnlyTheDocumentedRoutesBypassAccess, because that test names
// the list and this one proves the route actually behaves.
func TestMCPIsNotAnAccessBypass(t *testing.T) {
	h := routes(mcpCfg(), nil, &authn.StaticAuthenticator{}, nil, quiet())
	rr := httptest.NewRecorder()
	// A well-formed initialize, which a bypassed route would answer.
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)))
	if rr.Code == http.StatusOK {
		t.Fatalf("an unauthenticated initialize was answered; /mcp has become a bypass: %s", rr.Body.String())
	}
}

// A personal API bearer token must not work on /mcp even though it works on
// /v1. The whole point of the connector is a credential a person authorised in
// a browser and an administrator can revoke in one place; accepting a pasted
// token would reintroduce what ADR-0025 could not reach.
func TestMCPDoesNotAcceptABearerToken(t *testing.T) {
	h := routes(mcpCfg(), nil, &authn.StaticAuthenticator{}, nil, quiet())
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wgp_pretend_this_is_valid")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Fatal("a bearer token was accepted on /mcp")
	}
}

// Origin validation, per the MCP spec's DNS-rebinding protection.
//
// The attack is a page on evil.example scripting a request to our origin and
// reading the answer. That page cannot suppress its own Origin header, which is
// why an ABSENT header is allowed — most MCP clients are not browsers and send
// none, and requiring it would refuse them while looking like a working guard.
func TestOriginValidation(t *testing.T) {
	cfg := mcpCfg()
	for _, tc := range []struct {
		name, origin string
		allowed      bool
	}{
		{"our own app", appBase, true},
		{"our app, case-insensitive scheme and host", "HTTPS://WORK-STAGING.OPENBASES.COM", true},
		{"absent, which is most clients", "", true},
		{"another site", "https://evil.example", false},
		{"our host over plain http", "http://work-staging.openbases.com", false},
		{"our host as a prefix of theirs", "https://work-staging.openbases.com.evil.example", false},
		{"our host with a port", "https://work-staging.openbases.com:8443", false},
		{"a subdomain", "https://mcp.work-staging.openbases.com", false},
		{"nonsense", "not a url", false},
		{"null, which some sandboxed frames send", "null", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.origin == "" {
				// The absent case is about the guard letting it through, which
				// allowedOrigin never sees.
				return
			}
			if got := allowedOrigin(cfg, tc.origin); got != tc.allowed {
				t.Errorf("allowedOrigin(%q) = %v, want %v", tc.origin, got, tc.allowed)
			}
		})
	}
}

// With nothing configured to compare against, refuse. An unconfigured
// allow-list that permits everything is the failure the guard exists to
// prevent, and WG_APP_BASE_URL is already required for the device flow.
func TestAnUnconfiguredAppURLRefusesEveryOrigin(t *testing.T) {
	cfg := config.ControlAPI{Environment: config.EnvLocal}
	for _, o := range []string{appBase, "https://evil.example"} {
		if allowedOrigin(cfg, o) {
			t.Errorf("with no WG_APP_BASE_URL set, %q was allowed", o)
		}
	}
}

// The guard runs before the transport, so a wrong Origin is refused whether or
// not the caller has an identity — and it says so in a code a client can read,
// because a wrong Origin allow-list otherwise fails silently for browsers.
func TestAWrongOriginIsRefusedWithACode(t *testing.T) {
	h := originGuard(mcpCfg(), quiet(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the transport was reached with a refused origin")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://evil.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("a refused origin returned %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "origin_refused") {
		t.Errorf("the refusal carries no code a client can branch on: %s", rr.Body.String())
	}
}

func TestOurOwnOriginAndAnAbsentOriginBothReachTheTransport(t *testing.T) {
	for _, origin := range []string{appBase, ""} {
		reached := false
		h := originGuard(mcpCfg(), quiet(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		if !reached {
			t.Errorf("origin %q did not reach the transport", origin)
		}
	}
}

// Sessions are bounded in time and live only in this process. A restart drops
// them and clients reconnect; putting them in Postgres would make a transport
// detail durable, and the thing worth keeping across a restart — who the person
// is — lives in the Access token rather than in the session.
func TestSessionsAreBoundedAndInMemory(t *testing.T) {
	if mcpSessionTTL <= 0 {
		t.Error("an idle session never expires, so a client that vanishes holds one forever")
	}
	if mcpSessionTTL > time.Hour {
		t.Errorf("the idle session TTL is %v; long enough that a leak is invisible", mcpSessionTTL)
	}
	// No table, no migration, no store. Asserted by reading the source, because
	// the property is an absence and an absence has nothing to call.
	src := readSource(t, "mcp.go")
	for _, forbidden := range []string{"INSERT INTO", "sql.DB", "db *sql"} {
		if strings.Contains(src, forbidden) {
			t.Errorf("mcp.go mentions %q; sessions are meant to be in memory only", forbidden)
		}
	}
}

// The body limit matches the rest of the API rather than being unbounded.
func TestTheMCPBodyIsBounded(t *testing.T) {
	if maxMCPBody <= 0 || maxMCPBody > 4<<20 {
		t.Errorf("maxMCPBody is %d; a JSON-RPC message should be bounded and small", maxMCPBody)
	}
}

// No OAuth is implemented here, and that is the design rather than an omission.
// If this file grows an authorisation or token endpoint, something has gone
// wrong: the credential is minted by an interactive Access session precisely so
// that revoking the person's access revokes the tool call.
func TestNoOAuthServerLivesInTheOrigin(t *testing.T) {
	src := readSource(t, "mcp.go")
	// Endpoint paths and protocol parameters, not bare words: "register"
	// alone matches registerMCP, which is this file's own entry point.
	for _, forbidden := range []string{
		"/oauth/authorize", "/oauth/token", "/.well-known/oauth-authorization-server",
		"/oauth/register", "client_secret", "grant_type=", "code_verifier",
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("mcp.go mentions %q — Access Managed OAuth issues the token; "+
				"the origin must not implement one (ADR-0028)", forbidden)
		}
	}
}

// The in-process caller dispatches into the authenticated /v1 chain with the
// identity attached, and does NOT make an HTTP request back through Cloudflare.
func TestToolCallsDispatchInProcessWithTheIdentity(t *testing.T) {
	var sawUser, sawPath, sawKey string
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		sawUser, sawPath, sawKey = id.UserID, r.URL.RequestURI(), r.Header.Get("Idempotency-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	p := &inProcess{dispatch: dispatch, id: authn.Identity{UserID: "user-1", Email: "a@b.c"}}

	status, body, err := p.Call(t.Context(), http.MethodGet, "/v1/inbox", nil)
	if err != nil {
		t.Fatalf("the in-process call failed: %v", err)
	}
	if status != http.StatusOK || !strings.Contains(string(body), "items") {
		t.Errorf("got %d %s", status, body)
	}
	if sawUser != "user-1" {
		t.Errorf("the dispatcher saw user %q; the identity did not travel", sawUser)
	}
	if sawPath != "/v1/inbox" {
		t.Errorf("the dispatcher saw path %q", sawPath)
	}
	if sawKey != "" {
		t.Errorf("a GET carried an idempotency key %q; only writes need one", sawKey)
	}
}

// Every write carries a fresh idempotency key, so one call retried by the
// transport is not two planning jobs — and two deliberate calls are two jobs,
// which is why the key is random rather than derived from the brief.
func TestWritesCarryAFreshIdempotencyKey(t *testing.T) {
	seen := map[string]bool{}
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := r.Header.Get("Idempotency-Key")
		if k == "" {
			t.Error("a write carried no idempotency key")
		}
		if seen[k] {
			t.Errorf("the key %q was reused; two deliberate calls would collapse into one", k)
		}
		seen[k] = true
		_, _ = w.Write([]byte(`{}`))
	})
	p := &inProcess{dispatch: dispatch, id: authn.Identity{UserID: "user-1"}}
	for range 3 {
		if _, _, err := p.Call(t.Context(), http.MethodPost, "/v1/work/plan",
			map[string]any{"brief": "x"}); err != nil {
			t.Fatalf("call: %v", err)
		}
	}
	if len(seen) != 3 {
		t.Errorf("%d distinct keys for 3 calls", len(seen))
	}
}

// The tool set served remotely is the same one the CLI serves. Two tool sets
// would drift, and a remote list that lags by a release is a confidently wrong
// prompt rather than a missing feature.
func TestTheRemoteToolSetIsTheSharedOne(t *testing.T) {
	if len(wgmcp.Tools()) != len(wgmcp.Routes) {
		t.Fatalf("%d tools, %d routes", len(wgmcp.Tools()), len(wgmcp.Routes))
	}
	if len(wgmcp.Tools()) == 0 {
		t.Fatal("no tools are advertised")
	}
}

// readSource reads a file in this package, for the assertions whose property is
// an absence — an absence has nothing to call.
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// A request shaped like the one cloudflared delivers must reach the transport.
//
// THIS IS THE BUG THAT MADE THE CONNECTOR SHOW "no tools available", and it is
// worth the length of this comment because the symptom pointed nowhere near the
// cause.
//
// The OAuth flow completed, Claude connected, and every POST /mcp came back 403
// with no identity in the log:
//
//	{"msg":"request","method":"POST","path":"/mcp","status":403,"bytes":60,
//	 "subject":"-","user":"-"}
//
// Sixty bytes is exactly `Forbidden: invalid Host header
// "work-staging.openbases.com"` plus a newline, which is the SDK's own DNS
// rebinding protection. It fires when the SERVER's local address is loopback
// and the Host header is not:
//
//	if util.IsLoopback(localAddr.String()) && !util.IsLoopback(req.Host)
//
// control-api binds 127.0.0.1:8080 by design — cloudflared is the only ingress,
// and nothing else may reach it — and it serves a public hostname. So the
// condition is permanently true here, and every request through the tunnel was
// refused.
//
// The protection is aimed at a local MCP server on a developer's machine, where
// a page can resolve a name to 127.0.0.1 and reach a server that trusts its own
// loopback. That is not this: the only route in is the tunnel, and Access
// authenticates before we see anything.
//
// The FIRST attempt to reproduce this passed, wrongly, because
// httptest.NewRequest sets RemoteAddr and no LocalAddrContextKey — and the
// check reads the LOCAL address. A test that cannot fail is worse than none, so
// the local address is injected here explicitly.
func TestARequestShapedLikeTheTunnelReachesTheTransport(t *testing.T) {
	cfg := config.ControlAPI{Environment: config.EnvLocal, AppBaseURL: appBase}
	h := routes(cfg, nil, &authn.StaticAuthenticator{Identity: authn.Identity{
		Subject: "sub-1", Email: "a@b.c", UserID: "user-1",
	}}, nil, quiet())

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+
			`{"protocolVersion":"2025-06-18","capabilities":{},`+
			`"clientInfo":{"name":"probe","version":"1"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Host = "work-staging.openbases.com"
	// What net/http puts there when the listener is bound to loopback, which is
	// how this process runs in every environment.
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code == http.StatusForbidden {
		t.Fatalf("a tunnel-shaped request was refused: %d %s",
			rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if !strings.Contains(rr.Body.String(), "protocolVersion") {
		t.Errorf("initialize did not negotiate: %s", rr.Body.String())
	}
}

// Disabling the SDK's loopback check must not disable OUR origin check, which
// is the protection that actually applies to a tunnelled server.
func TestOurOriginGuardStillRefusesAForeignOriginThroughTheTunnel(t *testing.T) {
	cfg := config.ControlAPI{Environment: config.EnvLocal, AppBaseURL: appBase}
	h := routes(cfg, nil, &authn.StaticAuthenticator{Identity: authn.Identity{
		Subject: "sub-1", Email: "a@b.c", UserID: "user-1",
	}}, nil, quiet())

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://evil.example")
	req.Host = "work-staging.openbases.com"
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("a foreign origin returned %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "origin_refused") {
		t.Errorf("refused, but not by our guard: %s", rr.Body.String())
	}
}
