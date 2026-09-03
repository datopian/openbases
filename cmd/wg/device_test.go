package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The happy path: ask, poll while pending, collect once approved.
//
// Pending is the normal answer for most of this flow's life, so a client that
// treats the first 400 as failure never completes it -- which is the mistake
// this asserts against.
func TestLoginDevicePollsThroughPendingAndStoresTheToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var polls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/device/code":
			json.NewEncoder(w).Encode(map[string]any{
				"device_code":               "wgd_abc",
				"user_code":                 "ABCD-2345",
				"verification_uri":          "https://work.example/device",
				"verification_uri_complete": "https://work.example/device?code=ABCD-2345",
				"expires_in":                600,
				// Two seconds, so the test is not slow. The client floors
				// anything under two at five, so this exercises the floor too.
				"interval": 2,
			})
		case "/v1/device/token":
			if atomic.AddInt32(&polls, 1) < 2 {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "wgp_real", "expires_in": 28800, "scope": "project.read work.create",
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	code, err := loginDevice(srv.URL)
	if err != nil {
		t.Fatalf("loginDevice: %v", err)
	}
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if got := atomic.LoadInt32(&polls); got < 2 {
		t.Fatalf("polled %d times; it did not wait through pending", got)
	}

	c, err := loadCredentials()
	if err != nil {
		t.Fatalf("the token was not stored: %v", err)
	}
	if c.Token != "wgp_real" {
		t.Fatalf("stored %q", c.Token)
	}
}

// expired_token and invalid_grant are final. A client that retried them would
// spin until the deadline and then report the wrong reason.
func TestLoginDeviceStopsOnFinalStates(t *testing.T) {
	for _, state := range []string{"expired_token", "invalid_grant"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			var polls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/device/code" {
					json.NewEncoder(w).Encode(map[string]any{
						"device_code": "wgd_abc", "user_code": "ABCD-2345",
						"verification_uri": "https://work.example/device",
						"expires_in":       600, "interval": 2,
					})
					return
				}
				atomic.AddInt32(&polls, 1)
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]any{"error": state})
			}))
			defer srv.Close()

			if _, err := loginDevice(srv.URL); err == nil {
				t.Fatal("a final state was treated as retryable")
			}
			if got := atomic.LoadInt32(&polls); got != 1 {
				t.Fatalf("polled %d times for a final state; it should stop at one", got)
			}
		})
	}
}

// A refused scope comes back from the START call, before anybody is asked to
// approve anything, and the message names what was refused.
func TestLoginDeviceSurfacesARefusedScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"error": "a token cannot hold that action, so a device grant cannot request it",
			"code":  "scope_refused",
		})
	}))
	defer srv.Close()

	_, err := loginDevice(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "cannot hold that action") {
		t.Fatalf("error = %v", err)
	}
}
