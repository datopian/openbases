package authn

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Composed, never a literal: a token-shaped constant in the repository is
// flagged by the secret scan and, once pushed, keeps failing CI after it leaves
// the working tree (wg-cjp).
func chainFixture(seed string) string {
	raw := sha256.Sum256([]byte("wg-p4h.3 fixture/" + seed))
	return "wgp" + "_" + base64.RawURLEncoding.EncodeToString(raw[:])
}

type fakeLookup struct {
	secret   string
	tokenID  string
	userID   string
	scopes   []string
	recorded []string
	recErr   error
}

func (f *fakeLookup) Authenticate(_ context.Context, secret string) (string, string, []string, error) {
	if secret != f.secret {
		return "", "", nil, errors.New("no live token matches")
	}
	return f.tokenID, f.userID, f.scopes, nil
}

func (f *fakeLookup) RecordUse(_ context.Context, tokenID string) error {
	f.recorded = append(f.recorded, tokenID)
	return f.recErr
}

func bearerReq(header string) *http.Request {
	r := httptest.NewRequest("GET", "/v1/inbox", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	return r
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newFake() *fakeLookup {
	return &fakeLookup{
		secret:  chainFixture("ok"),
		tokenID: "tok-1",
		userID:  "11111111-1111-1111-1111-111111111111",
		scopes:  []string{"work.create"},
	}
}

func TestBearerTokenResolvesToItsOwnerAndNotAService(t *testing.T) {
	f := newFake()
	b := &BearerAuthenticator{Lookup: f, Log: quietLog()}

	id, err := b.Authenticate(context.Background(), bearerReq("Bearer "+f.secret))
	if err != nil {
		t.Fatalf("a valid token was refused: %v", err)
	}
	if id.UserID != f.userID {
		t.Fatalf("UserID = %q, want %q", id.UserID, f.userID)
	}
	// The decisive one. internal/domain/resolver.go returns early for a service
	// identity and leaves UserID empty, which is why a service token cannot read
	// one row of anyone's inbox. A personal token must not take that branch.
	if id.IsService {
		t.Fatal("a personal token authenticated as a service")
	}
	if !id.ViaToken() || id.TokenID != "tok-1" {
		t.Fatalf("TokenID = %q, ViaToken=%v", id.TokenID, id.ViaToken())
	}
	if len(f.recorded) != 1 {
		t.Fatalf("use recorded %d times, want 1", len(f.recorded))
	}
}

// A bookkeeping failure must not become a refused request.
func TestRecordUseFailureDoesNotRefuseTheRequest(t *testing.T) {
	f := newFake()
	f.recErr = errors.New("database is having a moment")
	b := &BearerAuthenticator{Lookup: f, Log: quietLog()}

	if _, err := b.Authenticate(context.Background(), bearerReq("Bearer "+f.secret)); err != nil {
		t.Fatalf("a valid token was refused because last_used_at could not be written: %v", err)
	}
}

func TestNoAuthorizationHeaderFallsThrough(t *testing.T) {
	b := &BearerAuthenticator{Lookup: newFake(), Log: quietLog()}
	_, err := b.Authenticate(context.Background(), bearerReq(""))
	if !errors.Is(err, ErrNotRecognised) {
		t.Fatalf("expected ErrNotRecognised, got %v", err)
	}
}

func TestAnotherSchemeFallsThrough(t *testing.T) {
	b := &BearerAuthenticator{Lookup: newFake(), Log: quietLog()}
	_, err := b.Authenticate(context.Background(), bearerReq("Basic dXNlcjpwYXNz"))
	if !errors.Is(err, ErrNotRecognised) {
		t.Fatalf("expected ErrNotRecognised, got %v", err)
	}
}

// A wrong token is a refusal, NOT a fall-through. wg-p4h.1 found this: if the
// two are the same, a mistyped token reaches the next link and the caller is
// told their Access JWT is missing.
func TestAWrongTokenIsRefusedNotFallenThrough(t *testing.T) {
	b := &BearerAuthenticator{Lookup: newFake(), Log: quietLog()}
	_, err := b.Authenticate(context.Background(), bearerReq("Bearer "+chainFixture("wrong")))
	if errors.Is(err, ErrNotRecognised) {
		t.Fatal("a wrong token fell through; the caller would be told the wrong credential failed")
	}
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

// A misconfiguration must not look like a missing credential.
func TestNoStoreConfiguredRefusesRatherThanFallsThrough(t *testing.T) {
	b := &BearerAuthenticator{Lookup: nil, Log: quietLog()}
	_, err := b.Authenticate(context.Background(), bearerReq("Bearer "+chainFixture("ok")))
	if errors.Is(err, ErrNotRecognised) {
		t.Fatal("an unconfigured token store fell through, so an operator would go and check their token")
	}
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

// AccessOrNotRecognised turns "no Access assertion" into a fall-through, and
// leaves everything else exactly as the validator decided.
func TestAccessFallsThroughOnlyWhenNoAssertionIsPresent(t *testing.T) {
	inner := &StaticAuthenticator{Identity: Identity{Subject: "s1", Email: "a@b.c"}}
	a := AccessOrNotRecognised{Inner: inner}

	if _, err := a.Authenticate(context.Background(), bearerReq("")); !errors.Is(err, ErrNotRecognised) {
		t.Fatalf("no assertion should fall through, got %v", err)
	}

	withHeader := bearerReq("")
	withHeader.Header.Set("Cf-Access-Jwt-Assertion", "a.b.c")
	if _, err := a.Authenticate(context.Background(), withHeader); err != nil {
		t.Fatalf("an assertion present should reach the validator: %v", err)
	}

	withCookie := bearerReq("")
	withCookie.AddCookie(&http.Cookie{Name: "CF_Authorization", Value: "a.b.c"})
	if _, err := a.Authenticate(context.Background(), withCookie); err != nil {
		t.Fatalf("a cookie assertion should reach the validator: %v", err)
	}
}

// The order that matters: a request carrying BOTH an Access assertion and a
// bearer token is judged by Access, and a refusal there is final.
func TestARefusedSessionIsNotRescuedByAToken(t *testing.T) {
	f := newFake()
	refusing := &StaticAuthenticator{} // zero Identity -> ErrUnauthenticated
	c := Chain{
		AccessOrNotRecognised{Inner: refusing},
		&BearerAuthenticator{Lookup: f, Log: quietLog()},
	}

	r := bearerReq("Bearer " + f.secret)
	r.Header.Set("Cf-Access-Jwt-Assertion", "a.b.c")

	if _, err := c.Authenticate(context.Background(), r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected the Access refusal to stand, got %v", err)
	}
	if len(f.recorded) != 0 {
		t.Fatal("the token link ran after Access refused, so a revoked session could be rescued by a token")
	}
}

func TestChainReachesTheTokenWhenNoSessionIsPresent(t *testing.T) {
	f := newFake()
	c := Chain{
		AccessOrNotRecognised{Inner: &StaticAuthenticator{}},
		&BearerAuthenticator{Lookup: f, Log: quietLog()},
	}
	id, err := c.Authenticate(context.Background(), bearerReq("Bearer "+f.secret))
	if err != nil {
		t.Fatalf("a tool's request was refused: %v", err)
	}
	if id.UserID != f.userID {
		t.Fatalf("UserID = %q", id.UserID)
	}
}

func TestEmptyChainAuthenticatesNobody(t *testing.T) {
	if _, err := (Chain{}).Authenticate(context.Background(), bearerReq("Bearer x")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("an empty chain must fail closed, got %v", err)
	}
}

// An authenticator returning a zero Identity with a nil error would
// authenticate a request as user "". The interface forbids it; the chain does
// not rely on that having been honoured.
type sloppy struct{}

func (sloppy) Authenticate(context.Context, *http.Request) (Identity, error) {
	return Identity{}, nil
}

func TestChainRefusesAnIdentityWithNoSubject(t *testing.T) {
	_, err := Chain{sloppy{}}.Authenticate(context.Background(), bearerReq("Bearer x"))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a subjectless identity was accepted: %v", err)
	}
}

// Scopes are a ceiling on the credential, never a grant to the person.
func TestScopesNarrowTokensAndNeverWidenSessions(t *testing.T) {
	session := Identity{UserID: "u1"}
	if !session.Permits("agent.dispatch") {
		t.Fatal("a session was narrowed by a scope it does not carry")
	}

	tok := Identity{UserID: "u1", TokenID: "t1", Scopes: []string{"project.read"}}
	if tok.Permits("agent.dispatch") {
		t.Fatal("a token permitted an action outside its scopes")
	}
	if !tok.Permits("project.read") {
		t.Fatal("a token was refused an action inside its scopes")
	}

	empty := Identity{UserID: "u1", TokenID: "t1"}
	if empty.Permits("project.read") {
		t.Fatal("a token with no scopes permitted a scoped action")
	}
}
