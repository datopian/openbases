package main

import (
	"net/http"
	"strings"
	"testing"
)

// Exit codes are an interface: a skill branches on them, and it cannot branch on
// prose. Each mapping is a promise to whoever wrote that branch.
func TestExitCodesDistinguishWhatAScriptMustActOn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"ok", http.StatusOK, `{}`, exitOK},
		{"created", http.StatusCreated, `{}`, exitOK},
		{"bad token", http.StatusUnauthorized, `{"error":"unauthorized"}`, exitUnauthenticated},
		{"rate limited", http.StatusTooManyRequests, `{"code":"rate_limited"}`, exitRateLimited},
		{"idempotency conflict", http.StatusConflict, `{"code":"idempotency_key_conflict"}`, exitConflict},

		// Two different problems wearing one status. A scope or role refusal is
		// fixed by changing the credential; a policy refusal is fixed by asking
		// a human. A script that cannot tell them apart retries the second
		// forever.
		{"token scope", http.StatusForbidden, `{"code":"token_scope_insufficient"}`, exitForbidden},
		{"role grant", http.StatusForbidden, `{"code":"role_grant_missing"}`, exitForbidden},
		{"token self-service", http.StatusForbidden, `{"code":"token_self_service_forbidden"}`, exitForbidden},
		{"spend cap", http.StatusForbidden, `{"code":"spend_cap_reached"}`, exitPolicyRefused},
		{"approval policy", http.StatusForbidden, `{"policy":"no_self_approval"}`, exitPolicyRefused},

		{"server error", http.StatusInternalServerError, `{}`, exitError},
		{"not implemented", http.StatusNotImplemented, `{"code":"not_implemented"}`, exitError},
	} {
		if got := codeFor(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A key that is not unique would make two different requests collide and replay
// each other's responses, which is worse than having no key at all.
func TestIdempotencyKeysAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 256; i++ {
		k := newIdempotencyKey()
		if k == "" {
			t.Fatal("generated an empty key")
		}
		if seen[k] {
			t.Fatal("generated a duplicate idempotency key")
		}
		seen[k] = true
	}
}

func TestLoginRefusesSomethingThatIsNotAWorkgraphToken(t *testing.T) {
	t.Setenv("WG_TOKEN", "ghp"+"_"+strings.Repeat("0123456789", 4))
	code, err := login(nil)
	if err == nil {
		t.Fatal("a token with the wrong prefix was stored; the operator would learn on the first 401 " +
			"and go looking at the server")
	}
	if code != exitUsage {
		t.Fatalf("exit %d, want %d", code, exitUsage)
	}
}

func TestLoginRefusesAnEmptyToken(t *testing.T) {
	t.Setenv("WG_TOKEN", "   ")
	if _, err := login(nil); err == nil {
		t.Fatal("an empty token was accepted")
	}
}
