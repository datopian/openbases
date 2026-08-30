package workspace

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type harness struct {
	key    *rsa.PrivateKey
	server *httptest.Server
	v      *PushValidator
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: key.Public(), KeyID: "test", Algorithm: "RS256", Use: "sig",
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(srv.Close)
	h := &harness{key: key, server: srv}
	h.v = &PushValidator{
		Audience:       "https://work.example/v1/google/events",
		ServiceAccount: "pusher@datopian-workgraph-events.iam.gserviceaccount.com",
		HTTPClient:     srv.Client(),
		Now:            func() time.Time { return now },
		testJWKS:       srv.URL,
	}
	return h
}

func (h *harness) token(t *testing.T, mutate func(*jwt.Claims, *pushClaims)) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: h.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"))
	if err != nil {
		t.Fatal(err)
	}
	std := jwt.Claims{
		Issuer:   GoogleIssuer,
		Audience: jwt.Audience{h.v.Audience},
		Expiry:   jwt.NewNumericDate(now.Add(time.Hour)),
		IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)),
	}
	extra := pushClaims{Email: h.v.ServiceAccount, EmailVerified: true}
	if mutate != nil {
		mutate(&std, &extra)
	}
	s, err := jwt.Signed(signer).Claims(std).Claims(extra).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAGenuinePushDeliveryIsAccepted(t *testing.T) {
	h := newHarness(t)
	if err := h.v.Verify(context.Background(), "Bearer "+h.token(t, nil)); err != nil {
		t.Fatalf("a valid delivery was rejected: %v", err)
	}
}

// The audience is what makes this prove the delivery was meant for US, rather
// than merely that it came from Google. A token minted for another
// Google-fronted service must not validate here.
func TestATokenForAnotherAudienceIsRefused(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, func(c *jwt.Claims, _ *pushClaims) {
		c.Audience = jwt.Audience{"https://someone-else.example/hook"}
	})
	if err := h.v.Verify(context.Background(), "Bearer "+tok); err == nil {
		t.Error("a token for another audience was accepted")
	}
}

// An empty audience is worse than no check: it accepts every Google-signed
// token while looking like verification.
func TestAnUnconfiguredAudienceRefusesEverything(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, nil)
	h.v.Audience = ""
	err := h.v.Verify(context.Background(), "Bearer "+tok)
	if err == nil {
		t.Fatal("an unconfigured validator accepted a token")
	}
	if !strings.Contains(err.Error(), "audience") {
		t.Errorf("the error should name the missing audience: %v", err)
	}
}

// A token Google signed for a different service account is a valid Google token
// and not one of ours.
func TestATokenFromAnotherServiceAccountIsRefused(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, func(_ *jwt.Claims, p *pushClaims) {
		p.Email = "someone-else@some-other-project.iam.gserviceaccount.com"
	})
	if err := h.v.Verify(context.Background(), "Bearer "+tok); err == nil {
		t.Error("a token from another service account was accepted")
	}
}

func TestAnExpiredTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, func(c *jwt.Claims, _ *pushClaims) {
		c.Expiry = jwt.NewNumericDate(now.Add(-time.Hour))
	})
	if err := h.v.Verify(context.Background(), "Bearer "+tok); err == nil {
		t.Error("an expired token was accepted")
	}
}

func TestATokenFromAnotherIssuerIsRefused(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, func(c *jwt.Claims, _ *pushClaims) { c.Issuer = "https://evil.example" })
	if err := h.v.Verify(context.Background(), "Bearer "+tok); err == nil {
		t.Error("a token from another issuer was accepted")
	}
}

// Signed by a key that is not Google's — the case an attacker controls.
func TestATokenSignedByAnotherKeyIsRefused(t *testing.T) {
	h := newHarness(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: other},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"))
	tok, _ := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer: GoogleIssuer, Audience: jwt.Audience{h.v.Audience},
		Expiry: jwt.NewNumericDate(now.Add(time.Hour)),
	}).Serialize()
	if err := h.v.Verify(context.Background(), "Bearer "+tok); err == nil {
		t.Error("a token signed by an unknown key was accepted")
	}
}

func TestGarbageIsRefused(t *testing.T) {
	h := newHarness(t)
	for _, s := range []string{"", "Bearer ", "not-a-jwt", "Bearer a.b.c",
		"Bearer eyJhbGciOiJub25lIn0..", "Basic dXNlcjpwYXNz"} {
		if err := h.v.Verify(context.Background(), s); err == nil {
			t.Errorf("%q was accepted", s)
		}
	}
}

// An empty key set must not be cached: doing so fails every delivery for an
// hour on one bad fetch.
func TestAnEmptyKeySetIsNotCached(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	defer srv.Close()
	// A properly signed token, so verification reaches the key fetch. A
	// malformed one fails at parsing and never asks for a key, which is what
	// the first version of this test actually measured.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"))
	tok, _ := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer: GoogleIssuer, Audience: jwt.Audience{"a"},
		Expiry: jwt.NewNumericDate(now.Add(time.Hour)),
	}).Serialize()

	v := &PushValidator{Audience: "a", HTTPClient: srv.Client(), testJWKS: srv.URL,
		Now: func() time.Time { return now }}
	for i := 0; i < 2; i++ {
		if err := v.Verify(context.Background(), "Bearer "+tok); err == nil {
			t.Fatal("accepted a token with no keys available")
		}
	}
	if calls < 2 {
		t.Errorf("the empty key set was cached: %d fetch(es) for 2 verifications", calls)
	}
	if calls == 0 {
		t.Error("the key endpoint was never called")
	}
}
