package geminiproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The proxy strips the client's upstream credentials, injects the gateway token,
// preserves attribution, and forwards to the gateway's google-ai-studio route at
// the right path -- the whole reason it exists.
func TestProxyRewritesAuthAndPath(t *testing.T) {
	var got *http.Request
	var gotBody string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	// Inject the test server's transport so the proxy trusts its self-signed cert.
	p, err := start(upstream.URL, "gw-secret-token", nil, upstream.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	req, _ := http.NewRequest("POST",
		p.BaseURL()+"/v1beta/models/gemini-3.8-flash:generateContent",
		strings.NewReader(`{"contents":[]}`))
	req.Header.Set("x-goog-api-key", "AIzaClientKeyThatMustNotLeak")
	req.Header.Set("Authorization", "Bearer client-bearer")
	req.Header.Set("cf-aig-metadata", `{"role":"polecat","bead":"wg-1"}`)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through proxy failed: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if got == nil {
		t.Fatal("upstream never received the request")
	}
	if got.Header.Get("x-goog-api-key") != "" {
		t.Error("x-goog-api-key was forwarded to the gateway; it must be stripped")
	}
	if got.Header.Get("Authorization") != "" {
		t.Error("Authorization was forwarded to the gateway; it must be stripped")
	}
	if got.Header.Get("cf-aig-authorization") != "Bearer gw-secret-token" {
		t.Errorf("cf-aig-authorization = %q, want the injected gateway token", got.Header.Get("cf-aig-authorization"))
	}
	if got.Header.Get("cf-aig-metadata") != `{"role":"polecat","bead":"wg-1"}` {
		t.Errorf("cf-aig-metadata not preserved: %q", got.Header.Get("cf-aig-metadata"))
	}
	if got.URL.Path != "/google-ai-studio/v1beta/models/gemini-3.8-flash:generateContent" {
		t.Errorf("path = %q, want the google-ai-studio route", got.URL.Path)
	}
	if gotBody != `{"contents":[]}` {
		t.Errorf("body not forwarded intact: %q", gotBody)
	}
}

func TestStartRejectsBadConfig(t *testing.T) {
	if _, err := Start("http://insecure.example", "t", nil); err == nil {
		t.Error("a non-https gateway base must be refused")
	}
	if _, err := Start("https://gw.example", "", nil); err == nil {
		t.Error("an empty token must be refused")
	}
}
