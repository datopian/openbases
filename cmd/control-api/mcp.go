package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/datopian/workgraph/internal/authn"
	"github.com/datopian/workgraph/internal/config"
	wgmcp "github.com/datopian/workgraph/internal/mcp"
	"github.com/datopian/workgraph/internal/version"
)

// The remote MCP server (wg-p4h.11).
//
// /mcp serves the same six tools as `wg mcp`, over Streamable HTTP, to the
// clients that cannot run a local process: Claude Cowork's sandbox, claude.ai
// in a browser, and the phone apps.
//
// AUTHENTICATION IS NOT IMPLEMENTED HERE, and that is the whole design. The
// route sits behind the same Cloudflare Access application as /v1 and the
// interface, with Access Managed OAuth switched on. A non-browser client that
// hits it without a credential is answered by ACCESS with a 401 and a
// WWW-Authenticate header pointing at Access's RFC 8414 discovery documents;
// the client opens the person's browser to the normal Google Workspace login,
// with MFA and the email allow-list; and the origin then receives
// Cf-Access-Jwt-Assertion, which internal/authn already verifies. Cloudflare's
// words: "no OAuth implementation is needed in your MCP server code".
//
// So there is no authorisation endpoint here, no token endpoint, no client
// registry and no dynamic client registration. If this file ever grows one,
// something has gone wrong: the credential is minted by an interactive Access
// session precisely so that revoking the person's access revokes the tool call.
//
// A personal API bearer token is NOT accepted on this path even though /v1
// accepts one. The point of the connector is a credential that a person
// authorised in a browser and an administrator can revoke in one place; a
// pasted token would reintroduce exactly what ADR-0025 could not reach.

// mcpSessionTTL bounds an idle session.
//
// Sessions live in memory in this process. A restart drops them and clients
// reconnect, which is the correct trade: putting them in Postgres would make a
// transport detail durable, and the thing worth keeping across a restart --
// who the person is -- is in the Access token, not in the session.
const mcpSessionTTL = 30 * time.Minute

// maxMCPBody bounds one JSON-RPC message. Generous enough for a real brief and
// small enough that a mistake is refused rather than buffered, matching maxBody
// in writes.go.
const maxMCPBody = 1 << 20

// registerMCP mounts the remote MCP transport.
//
// dispatch is the authenticated /v1 chain: rate limiting, spend caps, role
// checks and then the route table. Tool handlers call INTO it in process with
// the caller's identity already attached, rather than making an HTTPS request
// back to ourselves through Cloudflare.
//
// Routing internally through the existing chain rather than refactoring every
// /v1 handler into a callable function was chosen because it is much the
// smaller change AND the safer one: rate limits, spend caps, role checks,
// idempotency and row-level security all apply to a tool call exactly as they
// apply to the same request over HTTPS, because it IS the same request. The
// alternative -- lifting two dozen handler closures out of main.go -- would
// have been a large diff whose failure mode is a tool path that quietly skips
// one of those layers.
func registerMCP(mux *http.ServeMux, cfg config.ControlAPI, dispatch http.Handler,
	auth authn.Authenticator, resolver authn.Resolver, log *slog.Logger) {

	handler := sdk.NewStreamableHTTPHandler(
		func(r *http.Request) *sdk.Server {
			// A server per request, holding this caller's identity. The SDK
			// hands us the request precisely so the tool set can be bound to
			// who is asking.
			id, _ := authn.FromContext(r.Context())
			return wgmcp.NewServer(wgmcp.Options{
				Caller:   &inProcess{dispatch: dispatch, id: id},
				Version:  version.Version,
				HasShell: false,
				Observe: func(_ context.Context, tool string, refusal wgmcp.Refusal, err error) {
					// Identity and outcome. No arguments: a brief is a
					// paragraph of somebody's intent and it does not belong in
					// a log operators read (wg-90f).
					attrs := []any{
						"tool", tool,
						"user", id.UserID,
						"email", id.Email,
						"refusal", int(refusal),
					}
					switch {
					case err != nil:
						log.Error("mcp tool call failed", append(attrs, "error", err)...)
					case refusal != wgmcp.RefusalNone:
						log.Info("mcp tool call refused", attrs...)
					default:
						log.Info("mcp tool call", attrs...)
					}
				},
			})
		},
		&sdk.StreamableHTTPOptions{
			SessionTimeout: mcpSessionTTL,
			Logger:         log,
			// The SDK's own DNS-rebinding protection is off, and this is the
			// line that made the connector work at all.
			//
			// It refuses a request when the SERVER's local address is loopback
			// and the Host header is not:
			//
			//   if util.IsLoopback(localAddr.String()) && !util.IsLoopback(req.Host)
			//
			// control-api binds 127.0.0.1:8080 by design — cloudflared is the
			// only ingress and nothing else may reach the process — and it
			// serves a public hostname. So that condition is permanently true
			// here and EVERY request through the tunnel was answered
			//
			//   403 Forbidden: invalid Host header "work-staging.openbases.com"
			//
			// which reached the client as "This connector has no tools
			// available": the OAuth flow had succeeded, tools/list was refused,
			// and nothing in the message pointed at a Host header.
			//
			// The protection is aimed at a local MCP server on a developer's
			// machine, where a page can resolve a name to 127.0.0.1 and reach a
			// server that trusts its own loopback. That threat does not exist
			// here. The only route in is the tunnel, and Access authenticates
			// every request before the origin sees it — so a page cannot make
			// an authenticated request at all, whatever it resolves.
			//
			// What replaces it is not nothing: originGuard above enforces our
			// own Origin allow-list, which is the check that applies to a
			// tunnelled server, and a test asserts it still refuses a foreign
			// origin with this disabled.
			DisableLocalhostProtection: true,
		},
	)

	guarded := originGuard(cfg, log, http.MaxBytesHandler(handler, maxMCPBody))

	// Behind the same middleware as /v1, so an unauthenticated request that
	// somehow reaches the origin is still refused here. Access refuses it
	// first; this is the second lock, not the only one.
	//
	// "/mcp" without a trailing slash: one exact path, three methods. The SDK's
	// handler routes POST, GET (the SSE stream) and DELETE (session end)
	// itself, so registering the path once is registering all three.
	mux.Handle("/mcp", authn.Middleware(auth, resolver, log)(guarded))
}

// originGuard implements the MCP spec's DNS-rebinding protection.
//
// A browser-based client sends Origin; a native client usually sends none. Both
// are allowed, and anything else is refused -- because the attack this stops is
// a page on evil.example scripting a request to our origin and reading the
// answer, and that page cannot suppress its own Origin header.
//
// The absence of the header is allowed deliberately. Requiring it would refuse
// every non-browser client, which is most of them, and would look like a
// working guard while breaking the transport.
func originGuard(cfg config.ControlAPI, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && !allowedOrigin(cfg, o) {
			// Logged, because a wrong Origin allow-list fails SILENTLY for
			// browser clients: the page sees a refusal it cannot explain and
			// the origin says nothing. This is the line that makes it findable.
			log.Warn("mcp request refused for its origin", "origin", o)
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "this origin may not call the MCP endpoint",
				"code":  "origin_refused",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowedOrigin reports whether an Origin header may call /mcp.
//
// Only our own app URL. Not a wildcard and not "any https", because the whole
// value of the check is that it names one host.
func allowedOrigin(cfg config.ControlAPI, origin string) bool {
	base := strings.TrimSuffix(cfg.AppBaseURL, "/")
	if base == "" {
		// Nothing configured to compare against. Refuse rather than allow: an
		// unconfigured allow-list that permits everything is the failure this
		// guard exists to prevent, and WG_APP_BASE_URL is already required for
		// the device flow.
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Scheme+"://"+u.Host, base)
}

// inProcess is a wgmcp.Caller that dispatches into this process.
type inProcess struct {
	dispatch http.Handler
	id       authn.Identity
}

// Call runs one Workgraph operation through the authenticated /v1 chain.
func (p *inProcess) Call(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(b)
	}

	req := httptest.NewRequest(method, path, reader)
	req = req.WithContext(authn.NewContext(ctx, p.id))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// A fresh idempotency key per call.
	//
	// Per CALL, not per session: a model that asks to file the same brief twice
	// means it twice, and replaying the first answer would hide the second
	// request rather than protect it. The key exists so that ONE call retried
	// by the transport is not two planning jobs.
	if method != http.MethodGet {
		if key := newIdempotencyKey(); key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
	}

	rr := httptest.NewRecorder()
	p.dispatch.ServeHTTP(rr, req)
	return rr.Code, rr.Body.Bytes(), nil
}

// newIdempotencyKey returns a fresh key for one write.
//
// Random rather than derived from the arguments. A key derived from the brief
// would make "file this same brief again" a replay of the first answer, which
// is a decision the person made twice being honoured once.
func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Cannot happen on any platform we run on; if it does, a key that
		// collides is worse than none, so send none and let the write proceed
		// unprotected rather than dedupe against another call.
		return ""
	}
	return "mcp-" + hex.EncodeToString(b[:])
}
