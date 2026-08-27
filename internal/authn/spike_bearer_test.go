//go:build spike

package authn

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	spikeToken  = "wgp_spike_0123456789abcdef"
	spikeUserID = "11111111-1111-1111-1111-111111111111"
	spikeSubj   = "token:spike"
)

func newBearer() *BearerAuthenticator {
	return &BearerAuthenticator{
		TokenSHA256: sha256.Sum256([]byte(spikeToken)),
		Subject:     spikeSubj,
		UserID:      spikeUserID,
	}
}

func bearerRequest(header string) *http.Request {
	r := httptest.NewRequest("GET", "/v1/inbox", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	return r
}

// The question this spike exists to answer, at the identity layer.
func TestBearerTokenResolvesToAPersonNotAMachine(t *testing.T) {
	id, err := newBearer().Authenticate(context.Background(), bearerRequest("Bearer "+spikeToken))
	if err != nil {
		t.Fatalf("valid token refused: %v", err)
	}
	if id.UserID != spikeUserID {
		t.Fatalf("UserID = %q, want %q", id.UserID, spikeUserID)
	}
	// The decisive assertion. internal/domain/resolver.go returns early for
	// IsService and leaves UserID empty, which is why a service token cannot
	// read one row of anyone's inbox. A personal token must not take that
	// branch.
	if id.IsService {
		t.Fatal("a personal token authenticated as a service: it would inherit the machine branch and never acquire a user")
	}
}

func TestTokenAlteredByOneByteIsRefused(t *testing.T) {
	altered := []byte(spikeToken)
	altered[len(altered)-1] ^= 1
	_, err := newBearer().Authenticate(context.Background(), bearerRequest("Bearer "+string(altered)))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
	// It must be a refusal, not a fall-through: a mistyped token that reports
	// "not mine" sends the operator to debug the wrong credential.
	if errors.Is(err, errNotMine) {
		t.Fatal("a wrong token fell through instead of being refused")
	}
}

func TestSchemeIsCaseInsensitiveAndOtherSchemesFallThrough(t *testing.T) {
	if _, err := newBearer().Authenticate(context.Background(), bearerRequest("bEaReR "+spikeToken)); err != nil {
		t.Fatalf("lowercase scheme refused: %v", err)
	}
	_, err := newBearer().Authenticate(context.Background(), bearerRequest("Basic dXNlcjpwYXNz"))
	if !errors.Is(err, errNotMine) {
		t.Fatalf("a non-Bearer scheme should fall through, got %v", err)
	}
	_, err = newBearer().Authenticate(context.Background(), bearerRequest(""))
	if !errors.Is(err, errNotMine) {
		t.Fatalf("no header should fall through, got %v", err)
	}
}

// refuser recognises every request and always refuses, standing in for Access
// rejecting a token it did recognise.
type refuser struct{ called *bool }

func (f refuser) Authenticate(context.Context, *http.Request) (Identity, error) {
	*f.called = true
	return Identity{}, ErrUnauthenticated
}

// notMine never recognises anything, standing in for Access with no JWT present.
type notMine struct{ called *bool }

func (n notMine) Authenticate(context.Context, *http.Request) (Identity, error) {
	*n.called = true
	return Identity{}, errNotMine
}

func TestChainFallsThroughOnlyWhenNothingRecognised(t *testing.T) {
	var reached bool
	c := Chain{notMine{&reached}, newBearer()}
	id, err := c.Authenticate(context.Background(), bearerRequest("Bearer "+spikeToken))
	if err != nil {
		t.Fatalf("chain refused a valid bearer token: %v", err)
	}
	if !reached {
		t.Fatal("the first authenticator was skipped")
	}
	if id.UserID != spikeUserID {
		t.Fatalf("UserID = %q", id.UserID)
	}
}

func TestChainStopsAtTheFirstRecognisedRefusal(t *testing.T) {
	var called bool
	// Access recognises and refuses; the bearer link must NOT get a turn, or a
	// revoked Access session could be rescued by a token in the same request.
	c := Chain{refuser{&called}, newBearer()}
	_, err := c.Authenticate(context.Background(), bearerRequest("Bearer "+spikeToken))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected refusal, got %v", err)
	}
	if !called {
		t.Fatal("the refusing authenticator never ran")
	}
}

func TestEmptyChainAuthenticatesNobody(t *testing.T) {
	_, err := Chain{}.Authenticate(context.Background(), bearerRequest("Bearer "+spikeToken))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("an empty chain must fail closed, got %v", err)
	}
}

// spikeResolver reproduces internal/domain/resolver.go's ONLY provenance gate:
// a service identity is returned without a UserID, a person's is given one.
// Using the real Store would need PostgreSQL; the branch being reproduced is
// four lines and is quoted in the spike write-up.
type spikeResolver struct{ userID string }

func (s spikeResolver) Resolve(_ context.Context, id Identity) (Identity, error) {
	if id.IsService {
		return id, nil // no user record, and must not acquire one
	}
	if id.UserID == "" {
		id.UserID = s.userID
	}
	return id, nil
}

// The end-to-end shape: a bearer credential through the real middleware reaches
// a handler with a user, which is the precondition for authz.WithUser opening a
// session at all.
func TestBearerCredentialReachesAHandlerWithAUser(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var seen Identity
	h := Middleware(Chain{newBearer()}, spikeResolver{userID: spikeUserID}, log)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := FromContext(r.Context())
			if !ok {
				t.Error("handler ran without an identity")
			}
			seen = id
			w.WriteHeader(http.StatusOK)
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest("Bearer "+spikeToken))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen.UserID != spikeUserID {
		t.Fatalf("handler saw UserID %q, want %q", seen.UserID, spikeUserID)
	}
	if seen.IsService {
		t.Fatal("handler saw a service identity")
	}
}

// An Access identity and a bearer identity for the same person must be
// INDISTINGUISHABLE by the time anything downstream reads them.
//
// This is what makes the RLS answer follow. authz.WithUser takes a userID
// string and sets workgraph.user_id; current_app_user() reads that setting.
// There is no channel by which "how was this caller authenticated" reaches a
// policy — so if the two identities agree on UserID, every policy decides
// identically by construction.
func TestAccessAndBearerIdentitiesAreIndistinguishableDownstream(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	res := spikeResolver{userID: spikeUserID}

	capture := func(a Authenticator, r *http.Request) Identity {
		var got Identity
		Middleware(a, res, log)(http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
			got, _ = FromContext(rq.Context())
		})).ServeHTTP(httptest.NewRecorder(), r)
		return got
	}

	// StaticAuthenticator stands in for a validated Access JWT: it is the
	// existing local-development path and produces an Identity the same way.
	accessID := capture(&StaticAuthenticator{Identity: Identity{Subject: "access-subject", Email: "someone@datopian.com"}},
		httptest.NewRequest("GET", "/v1/inbox", nil))
	bearerID := capture(Chain{newBearer()}, bearerRequest("Bearer "+spikeToken))

	if accessID.UserID != bearerID.UserID {
		t.Fatalf("UserID differs: access %q, bearer %q", accessID.UserID, bearerID.UserID)
	}
	if accessID.IsService != bearerID.IsService {
		t.Fatalf("IsService differs: access %v, bearer %v", accessID.IsService, bearerID.IsService)
	}
	if bearerID.UserID == "" {
		t.Fatal("the bearer identity carries no user, so WithUser would refuse to open a session")
	}
}

func TestRefusalLeaksNothingAboutWhichCheckFailed(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := Middleware(Chain{newBearer()}, spikeResolver{userID: spikeUserID}, log)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler ran for an unauthenticated request")
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest("Bearer wgp_not_the_token"))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["error"] != "unauthorized" {
		t.Fatalf("body = %v, want only a generic refusal", body)
	}
}
