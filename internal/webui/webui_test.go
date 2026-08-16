package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	h := Handler()
	if h == nil {
		t.Skip("no web build embedded; run make web-build")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

func TestRootServesTheApp(t *testing.T) {
	rr := serve(t, "/")
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "<div id=\"root\"") {
		t.Error("the response does not look like the application shell")
	}
}

// A deep link must render the app, not 404.
//
// The client owns its routes, so refreshing on /projects/nged has to reach
// index.html — otherwise every shared link is broken for the person who
// receives it, which is exactly when links matter.
func TestDeepLinkServesTheApp(t *testing.T) {
	rr := serve(t, "/projects/nged")
	if rr.Code != http.StatusOK {
		t.Fatalf("a deep link returned %d; shared links would be broken", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "<div id=\"root\"") {
		t.Error("a deep link did not return the application shell")
	}
}

// A missing asset must 404 rather than fall through to the app.
//
// Serving index.html for a missing .js turns a broken build into a blank page
// with no console error — far harder to diagnose than a plain 404.
func TestMissingAssetDoesNotFallThroughToTheApp(t *testing.T) {
	rr := serve(t, "/assets/does-not-exist.js")
	if rr.Code == http.StatusOK && strings.Contains(rr.Body.String(), "<div id=\"root\"") {
		t.Fatal("a missing asset returned the app shell; a broken build would render as a blank page")
	}
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for a missing asset, got %d", rr.Code)
	}
}

// index.html must not be cached, or a deploy leaves clients on the old app
// indefinitely while the hashed bundles it references disappear.
func TestIndexIsNotCached(t *testing.T) {
	rr := serve(t, "/")
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("index.html Cache-Control is %q; clients would keep a stale app after a deploy", cc)
	}
}
