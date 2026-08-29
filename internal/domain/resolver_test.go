package domain

import (
	"context"
	"errors"
	"testing"

	"github.com/datopian/workgraph/internal/authn"
)

// A service token must never acquire a user record. If it did, a machine would
// inherit a person's project memberships and could act as them.
func TestResolverLeavesServiceTokensAlone(t *testing.T) {
	r := NewResolver(nil) // never reaches the store
	in := authn.Identity{Subject: "svc-1", IsService: true, ServiceName: "config-management"}

	out, err := r.Resolve(context.Background(), in)
	if err != nil {
		t.Fatalf("a service token should resolve without a user record: %v", err)
	}
	if out.UserID != "" {
		t.Errorf("a service token must not be given a user ID, got %q", out.UserID)
	}
	if !out.IsService {
		t.Error("the service marker must survive resolution")
	}
}

var _ = errors.Is

// An identity that already carries a UserID is returned untouched.
//
// This is the regression test for a bug that every unit test missed and one
// live request found. The bearer authenticator resolves a token by its row in
// api_tokens — the lookup IS the authentication — and hands back an identity
// with the UserID already set and a synthetic subject, "token:<id>". The
// resolver then discarded that UserID and looked the synthetic subject up
// against cloudflare_access identities, found nothing, and refused a valid
// token with ErrNoSuchUser.
//
// Every layer passed its own tests. Nothing tested the two together, so the
// first thing to exercise the whole path was a real token against staging
// (wg-p4h.1). The store is nil here deliberately: if this ever reaches it
// again, the test panics rather than quietly passing.
func TestResolverKeepsAUserIDAnAuthenticatorAlreadyEstablished(t *testing.T) {
	r := NewResolver(nil) // must never be dereferenced
	in := authn.Identity{
		Subject: "token:cfb4d751-81a6-4964-a410-6e13de1b9ff6",
		UserID:  "2a3008b7-8758-4be3-9c02-bea38d524a41",
		TokenID: "cfb4d751-81a6-4964-a410-6e13de1b9ff6",
	}

	out, err := r.Resolve(context.Background(), in)
	if err != nil {
		t.Fatalf("an already-resolved identity was refused: %v", err)
	}
	if out.UserID != in.UserID {
		t.Fatalf("UserID = %q, want %q — the resolver discarded what the authenticator established",
			out.UserID, in.UserID)
	}
	if out.TokenID != in.TokenID {
		t.Errorf("TokenID was lost: %q", out.TokenID)
	}
}

// A session identity carries no UserID, so it must still be looked up. Without
// this the fix above would turn into "trust whatever arrives", which is the
// opposite of what it means.
func TestResolverStillLooksUpAnIdentityWithNoUserID(t *testing.T) {
	r := NewResolver(nil)
	in := authn.Identity{Subject: "access-subject", Email: "someone@datopian.com"}

	defer func() {
		if recover() == nil {
			t.Fatal("an identity with no UserID did not reach the store; it would be admitted " +
				"as a user nobody looked up")
		}
	}()
	_, _ = r.Resolve(context.Background(), in)
}
