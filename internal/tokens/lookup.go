package tokens

import "context"

// Lookup adapts a Store to authn.TokenLookup.
//
// The adapter exists so internal/authn keeps no database import and can be
// tested without one. It also flattens the Identity struct into the values the
// authenticator needs, which keeps the interface in authn free of a type from
// this package — the dependency runs one way.
type Lookup struct{ store *Store }

// NewLookup returns a Lookup over store.
func NewLookup(store *Store) *Lookup { return &Lookup{store: store} }

// Authenticate resolves a presented secret to its owner.
//
// The error is deliberately uninformative: Store.Authenticate already refuses
// to distinguish "no such token" from revoked, expired, or a suspended owner,
// because telling them apart lets a caller learn whether a token ever existed.
func (l *Lookup) Authenticate(ctx context.Context, secret string) (tokenID, userID string, scopes []string, err error) {
	id, err := l.store.Authenticate(ctx, secret)
	if err != nil {
		return "", "", nil, err
	}
	return id.TokenID, id.UserID, id.Scopes, nil
}

// RecordUse notes that a token was used. Best-effort: the caller logs the error
// and continues, because refusing a valid request over a bookkeeping write is
// an outage and losing a timestamp is not.
func (l *Lookup) RecordUse(ctx context.Context, tokenID string) error {
	return l.store.RecordUse(ctx, tokenID)
}
