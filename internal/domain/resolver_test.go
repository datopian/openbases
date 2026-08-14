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
