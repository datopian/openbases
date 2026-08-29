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

	// An authenticator that already established the user has done the
	// resolution, and more authoritatively than this can.
	//
	// This function was written when Cloudflare Access was the only way in, so
	// it assumed every identity arrives as a provider subject to be looked up.
	// A personal API token is resolved by its own row in api_tokens — the
	// lookup IS the authentication — and it carries a synthetic subject
	// ("token:<id>") that no cloudflare_access identity will ever match.
	//
	// Without this, the resolver discarded a correct UserID, failed to find the
	// synthetic subject, and refused a perfectly valid token with
	// ErrNoSuchUser. Found by minting a real token against staging: every layer
	// passed its own tests and the request still 401'd, because nothing tested
	// the two together (wg-p4h.1).
	if id.UserID != "" {
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
