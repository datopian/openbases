package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// There must be no generic shell endpoint, now or later. This test is the
// tripwire: adding "run_shell" to the accepted set makes it fail.
func TestNoGenericShellAction(t *testing.T) {
	for a := range accepted {
		s := strings.ToLower(string(a))
		for _, forbidden := range []string{"shell", "exec", "run_command", "eval"} {
			if strings.Contains(s, forbidden) {
				t.Fatalf("agentd must not accept a general execution action, found %q", a)
			}
		}
	}
}

func TestJob_RejectsUnknownAction(t *testing.T) {
	rr := httptest.NewRecorder()
	body := strings.NewReader(`{"action":"run_shell","capability":"signed"}`)
	routes().ServeHTTP(rr, httptest.NewRequest("POST", "/internal/job", body))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown action must be rejected with 400, got %d", rr.Code)
	}
}

func TestJob_RequiresCapability(t *testing.T) {
	rr := httptest.NewRecorder()
	body := strings.NewReader(`{"action":"cell.health"}`)
	routes().ServeHTTP(rr, httptest.NewRequest("POST", "/internal/job", body))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("a job without a signed capability must be refused, got %d", rr.Code)
	}
}

func TestJob_KnownActionWithCapabilityIsNotImplemented(t *testing.T) {
	rr := httptest.NewRecorder()
	body := strings.NewReader(`{"action":"cell.health","capability":"signed"}`)
	routes().ServeHTTP(rr, httptest.NewRequest("POST", "/internal/job", body))

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 until WP-E1 lands, got %d", rr.Code)
	}
}
