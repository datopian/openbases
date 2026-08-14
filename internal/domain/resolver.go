package domain

import (
	"context"
	"errors"

	"github.com/datopian/workgraph/internal/authn"
)

// Resolver maps an authenticated Cloudflare Access identity to an application
// user, and refuses callers who have no user record.
//
// Authentication establishes who someone is at the identity edge. This decides
// whether they are a Workgraph user at all — the distinction that makes removing
// a user take effect immediately rather than when their token expires
// (WP-C2 acceptance).
type Resolver struct{ store *Store }

func NewResolver(s *Store) *Resolver { return &Resolver{store: s} }

// Resolve attaches the application user ID, or refuses.
func (r *Resolver) Resolve(ctx context.Context, id authn.Identity) (authn.Identity, error) {
	// A service token is a machine. It has no user record and must not acquire
	// one, or it would inherit a person's project memberships.
	if id.IsService {
		return id, nil
	}

	userID, _, err := r.store.UserBySubject(ctx, "cloudflare_access", id.Subject)
	if err == nil {
		id.UserID = userID
		return id, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return authn.Identity{}, err
	}

	// First sight of this subject. Link it to an existing user, which is the
	// only way an account can ever be usable: a Cloudflare Access subject is
	// issued by the provider and cannot be seeded in advance.
	//
	// If no user record matches the address, the caller is refused. Being in
	// the Access allow-list is not the same as being a Workgraph user.
	userID, err = r.store.LinkIdentityByEmail(ctx, "cloudflare_access", id.Subject, id.Email)
	if errors.Is(err, ErrNotFound) {
		return authn.Identity{}, authn.ErrNoSuchUser
	}
	if err != nil {
		return authn.Identity{}, err
	}

	id.UserID = userID
	return id, nil
}

var _ authn.Resolver = (*Resolver)(nil)
