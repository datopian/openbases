// Package geminiproxy is a loopback reverse proxy that lets an AI-SDK Google
// client reach Gemini through the Cloudflare AI Gateway's google-ai-studio route
// on UNIFIED BILLING.
//
// Why this exists. The gateway's native google-ai-studio route bills through the
// account's prepaid balance (unified billing) ONLY when the request carries just
// cf-aig-authorization and no upstream Google credential. If the client also
// sends x-goog-api-key or Authorization, the gateway forwards it to Google,
// which rejects it ("API key not valid") -- confirmed on staging 2026-09-14.
//
// The AI SDK Google provider that OpenCode uses ALWAYS attaches x-goog-api-key.
// So opencode cannot talk to the gateway's google route directly without either
// breaking (bad key) or putting a real Google key on the node (BYOK -- a second
// bill, and a provider key on an execution node, which the gateway design exists
// to avoid). This proxy sits between them: opencode points its Google provider
// at http://127.0.0.1:<port>, and the proxy strips the client's upstream auth
// and injects cf-aig-authorization before forwarding to the gateway. The gateway
// token stays in this process's memory, never in the opencode config file.
//
// The Gemini-native surface (unlike the gateway's OpenAI /compat surface) round-
// trips Gemini 3's thought_signature, which is what tool use needs.
package geminiproxy

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Proxy is a running loopback proxy. Close it when the run is done.
type Proxy struct {
	server *http.Server
	ln     net.Listener
	base   string // e.g. http://127.0.0.1:52144
}

// Start binds a loopback listener and serves until Close.
//
// gatewayBaseURL is the gateway prefix WITHOUT a provider segment, e.g.
// https://gateway.ai.cloudflare.com/v1/<account>/workgraph-staging-oss. token is
// the AI Gateway token; it is held in memory and sent as cf-aig-authorization,
// never logged.
func Start(gatewayBaseURL, token string, log *slog.Logger) (*Proxy, error) {
	return start(gatewayBaseURL, token, log, nil)
}

// start is Start with an injectable transport, so a test can point the proxy at
// an httptest TLS server whose certificate the default transport would reject.
// transport nil means the default.
func start(gatewayBaseURL, token string, log *slog.Logger, transport http.RoundTripper) (*Proxy, error) {
	base := strings.TrimSuffix(strings.TrimSpace(gatewayBaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("a gemini proxy needs the gateway base URL")
	}
	if !strings.HasPrefix(base, "https://") {
		// The token would otherwise cross the wire in clear. The gateway is
		// always https; a non-https base is a misconfiguration, not a mode.
		return nil, fmt.Errorf("the gateway base URL must be https, got %q", base)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("a gemini proxy needs the gateway token, or its traffic escapes every budget")
	}
	// The native provider route. The inbound path from the AI SDK Google client
	// (/v1beta/models/<model>:generateContent) is appended to this by SetURL.
	target, err := url.Parse(base + "/google-ai-studio")
	if err != nil {
		return nil, fmt.Errorf("gateway base URL is not a URL: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}

	rp := &httputil.ReverseProxy{
		// -1 flushes each write immediately, which streamGenerateContent (SSE)
		// needs: buffered, the agent would see the whole turn at once or time out.
		FlushInterval: -1,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target) // joins target + inbound path, sets the upstream Host
			// Strip every upstream credential the client attached. This is the
			// whole point: the gateway forwards these to Google and Google
			// rejects them, and unified billing only applies when they are
			// absent.
			r.Out.Header.Del("X-Goog-Api-Key")
			r.Out.Header.Del("Authorization")
			// Inject the gateway credential. cf-aig-metadata, when the client
			// set it, is left as-is so spend stays attributed.
			r.Out.Header.Set("cf-aig-authorization", "Bearer "+token)
			// Do not leak the caller's forwarded-for chain to the gateway.
			r.Out.Header.Del("X-Forwarded-For")
		},
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Error("gemini proxy could not reach the gateway", "error", err)
			http.Error(w, "gemini proxy upstream error", http.StatusBadGateway)
		},
	}

	if transport != nil {
		rp.Transport = transport
	}

	// Loopback only. Nothing off-box may reach a listener that turns any request
	// into a billed, authenticated call.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("gemini proxy could not listen: %w", err)
	}
	p := &Proxy{
		ln:   ln,
		base: "http://" + ln.Addr().String(),
		server: &http.Server{
			Handler:           rp,
			ReadHeaderTimeout: 30 * time.Second,
		},
	}
	go func() {
		if err := p.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("gemini proxy stopped", "error", err)
		}
	}()
	return p, nil
}

// BaseURL is the loopback address to point an AI-SDK Google provider at, e.g.
// http://127.0.0.1:52144. The provider appends /v1beta/models/... itself, so
// callers set the provider baseURL to BaseURL()+"/v1beta".
func (p *Proxy) BaseURL() string { return p.base }

// Close stops the proxy.
func (p *Proxy) Close() error {
	if p == nil || p.server == nil {
		return nil
	}
	return p.server.Close()
}
