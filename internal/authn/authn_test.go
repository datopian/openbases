package authn

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

// An unimplemented authenticator must deny, not allow. This is the single most
// important property of the package: a half-finished build must never
// authenticate anyone.
func TestAccessAuthenticator_DeniesByDefault(t *testing.T) {
	a := &AccessAuthenticator{TeamDomain: "datopian.cloudflareaccess.com", Audience: "aud"}
	r := httptest.NewRequest("GET", "/v1/me", nil)
	// A forged header must not help.
	r.Header.Set("Cf-Access-Authenticated-User-Email", "attacker@example.com")
	r.Header.Set("Cf-Access-Jwt-Assertion", "not-a-real-token")

	id, err := a.Authenticate(context.Background(), r)
	if err == nil {
		t.Fatal("unimplemented authenticator must not authenticate a request")
	}
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("unexpected error: %v", err)
	}
	if id.Subject != "" || id.UserID != "" {
		t.Errorf("no identity may be returned alongside an error: %+v", id)
	}
}

func TestStaticAuthenticator_RequiresSubject(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/me", nil)
	if _, err := (&StaticAuthenticator{}).Authenticate(context.Background(), r); err == nil {
		t.Fatal("an empty static identity must not authenticate")
	}
	s := &StaticAuthenticator{Identity: Identity{Subject: "sub-1", UserID: "user-1"}}
	id, err := s.Authenticate(context.Background(), r)
	if err != nil || id.UserID != "user-1" {
		t.Fatalf("got %+v, %v", id, err)
	}
}
