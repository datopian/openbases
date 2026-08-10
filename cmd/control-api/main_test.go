package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/datopian/workgraph/internal/config"
)

func TestHealthLive(t *testing.T) {
	rr := httptest.NewRecorder()
	routes(config.ControlAPI{Environment: config.EnvLocal}).
		ServeHTTP(rr, httptest.NewRequest("GET", "/health/live", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("liveness should be 200, got %d", rr.Code)
	}
}

// Readiness must not claim health it cannot verify. Reporting ready before the
// dependency checks exist would let a broken instance receive traffic.
func TestHealthReady_NotReadyUntilDependencyChecksExist(t *testing.T) {
	rr := httptest.NewRecorder()
	routes(config.ControlAPI{Environment: config.EnvLocal}).
		ServeHTTP(rr, httptest.NewRequest("GET", "/health/ready", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness should be 503 until WP-C1 lands, got %d", rr.Code)
	}
}

func TestUnknownRouteIsExplicitlyNotImplemented(t *testing.T) {
	rr := httptest.NewRecorder()
	routes(config.ControlAPI{Environment: config.EnvLocal}).
		ServeHTTP(rr, httptest.NewRequest("GET", "/v1/portfolio", nil))

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("an unimplemented route must say so, got %d", rr.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("response must be JSON: %v", err)
	}
	if body["error"] != "not implemented" {
		t.Errorf("unexpected body: %v", body)
	}
}
