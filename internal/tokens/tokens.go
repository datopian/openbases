// Package tokens issues and verifies personal API tokens: the credential a
// person gives a tool so it can reach Workgraph without a browser (wg-p4h.2).
//
// A token is not a second kind of principal. It resolves to the same user id a
// browser session resolves to, so authz.WithUser sets the same
// workgraph.user_id and every row-level security policy decides identically —
// which wg-p4h.1 established is structural rather than lucky: provenance is not
// an input a policy has.
//
// What this package guarantees:
//
//	the plaintext token exists exactly once, in the response to Mint, and is
//	never written to the database, a log, or an error;
//
//	revocation takes effect on the next request, with no cache in front of the
//	lookup to make "next" mean "eventually";
//
//	no protected action can be carried by a token, enforced by a CHECK
//	constraint rather than by remembering to validate here.
package tokens

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/datopian/openbases/internal/authz"
)

// Prefix marks a Workgraph personal token.
//
// It exists so a leaked token is greppable in a log scan and recognisable in a
// paste, and that is not theoretical: gitleaks flagged a wgp_-prefixed fixture
// in the wg-p4h.1 spike on the first CI run, with no rule written for it. The
// same property is why no token-shaped literal may appear in this repository,
// test fixtures included — see the tests, which compose theirs at run time.
const Prefix = "wgp_"

// MaxLifetime bounds how long a token may be valid for.
//
// Also a CHECK constraint in 0038_api_tokens.sql. Duplicated deliberately: here
// it produces a good error message, and there it is a rule that a hurried edit
// to this file cannot lift.
const MaxLifetime = 90 * 24 * time.Hour

// secretBytes is the entropy behind a token. 32 bytes is 256 bits, which is far
// past guessing, and the digest stored is the same width.
const secretBytes = 32

var (
	// ErrNotFound is returned when no live token matches a presented secret.
	// It is deliberately indistinguishable between "no such token", "revoked",
	// "expired" and "the owner is suspended": telling a caller which would let
	// them learn whether a token ever existed.
	ErrNotFound = errors.New("no live token matches")

	// ErrProtectedScope is returned when a mint requests an action that a token
	// may never carry.
	ErrProtectedScope = errors.New("a protected action cannot be granted to a token")

	// ErrLifetime is returned for a missing, past, or over-long expiry.
	ErrLifetime = errors.New("expiry must be in the future and within the maximum lifetime")

	// ErrUnknownScope is returned when a requested scope is not an action this
	// system has. Separate from ErrProtectedScope because the two deserve
	// different answers: a protected action is refused permanently, an unknown
	// one is nearly always a typo the caller can fix. Both are the caller's own
	// input, so both are safe to name back to them.
	ErrUnknownScope = errors.New("not an action this system has")
)

// Token is a stored token's metadata. It never carries the secret.
type Token struct {
	ID         string
	UserID     string
	Label      string
	Scopes     []string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Live reports whether the token would authenticate right now.
func (t Token) Live(now time.Time) bool {
	return t.RevokedAt == nil && t.ExpiresAt.After(now)
}

// Identity is what a presented secret resolves to.
type Identity struct {
	TokenID string
	UserID  string
	Scopes  []string
}

// Store issues and verifies tokens.
type Store struct {
	db *sql.DB
	// Now is overridable so tests do not sleep.
	Now func() time.Time
}

// NewStore returns a Store over db.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Mint creates a token for userID and returns the secret exactly once.
//
// The caller must already be authenticated as userID by an Access session.
// Minting from a token is refused a layer up, in the HTTP handler: a leaked
// token that can mint its own successor makes revocation meaningless.
func (s *Store) Mint(ctx context.Context, userID, label string, scopes []string, expiresAt time.Time, createdFrom string) (secret string, t Token, err error) {
	if strings.TrimSpace(label) == "" {
		return "", Token{}, errors.New("a token needs a label: it is the only thing that answers 'what breaks if I revoke this'")
	}
	now := s.now()
	if !expiresAt.After(now) || expiresAt.After(now.Add(MaxLifetime)) {
		return "", Token{}, fmt.Errorf("%w: at most %s from now", ErrLifetime, MaxLifetime)
	}
	if err := ValidateScopes(scopes); err != nil {
		return "", Token{}, err
	}

	secret, digest, err := generate()
	if err != nil {
		return "", Token{}, err
	}

	if scopes == nil {
		scopes = []string{}
	}

	err = authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`INSERT INTO api_tokens (user_id, label, token_sha256, scopes, expires_at, created_from)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 RETURNING id, created_at`,
			userID, label, digest[:], pgTextArray(scopes), expiresAt, nullable(createdFrom),
		).Scan(&t.ID, &t.CreatedAt)
	})
	if err != nil {
		// The secret is discarded rather than returned with an error. A caller
		// that stored it on a failed insert would hold a credential the
		// database has never heard of.
		return "", Token{}, fmt.Errorf("minting a token: %w", err)
	}

	t.UserID, t.Label, t.Scopes, t.ExpiresAt = userID, label, scopes, expiresAt
	return secret, t, nil
}

// Authenticate resolves a presented secret to its owner.
//
// It runs without a user, because the token is how the user is established.
// The lookup is a SECURITY DEFINER function that takes a digest and returns
// nothing an attacker could use to forge a second credential.
func (s *Store) Authenticate(ctx context.Context, secret string) (Identity, error) {
	digest, ok := digestOf(secret)
	if !ok {
		return Identity{}, ErrNotFound
	}

	var id Identity
	var scopes []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT token_id, user_id, scopes::text FROM system_api_token_authenticate($1)`,
		digest[:],
	).Scan(&id.TokenID, &id.UserID, &scopes)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Identity{}, ErrNotFound
	case err != nil:
		return Identity{}, fmt.Errorf("authenticating a token: %w", err)
	}
	id.Scopes = parseTextArray(string(scopes))
	return id, nil
}

// RecordUse updates last_used_at, coarsened by the database so a token in
// constant use is written to once per interval rather than once per request.
//
// Best-effort by contract: the error is returned for logging, and a caller must
// not fail a request on it. Losing a timestamp is a nuisance; refusing a valid
// request because a bookkeeping write failed is an outage.
func (s *Store) RecordUse(ctx context.Context, tokenID string) error {
	_, err := s.db.ExecContext(ctx, `SELECT system_api_token_record_use($1)`, tokenID)
	return err
}

// List returns a user's tokens, newest first, including revoked and expired
// ones. Hiding them would make "why did this stop working" unanswerable.
func (s *Store) List(ctx context.Context, userID string) ([]Token, error) {
	var out []Token
	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, user_id, label, scopes::text, created_at, expires_at, last_used_at, revoked_at
			   FROM api_tokens
			  ORDER BY created_at DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t Token
			var scopes []byte
			var lastUsed, revoked sql.NullTime
			if err := rows.Scan(&t.ID, &t.UserID, &t.Label, &scopes,
				&t.CreatedAt, &t.ExpiresAt, &lastUsed, &revoked); err != nil {
				return err
			}
			t.Scopes = parseTextArray(string(scopes))
			if lastUsed.Valid {
				v := lastUsed.Time
				t.LastUsedAt = &v
			}
			if revoked.Valid {
				v := revoked.Time
				t.RevokedAt = &v
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("listing tokens: %w", err)
	}
	return out, nil
}

// Revoke ends a token immediately. Revoking an already-revoked token is not an
// error: the caller wanted it dead and it is dead.
//
// Row-level security scopes this to the caller's own tokens, so a user id that
// does not own the token affects no rows and gets ErrNotFound.
func (s *Store) Revoke(ctx context.Context, userID, tokenID string) error {
	var affected int64
	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE api_tokens SET revoked_at = coalesce(revoked_at, now()) WHERE id = $1`, tokenID)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return fmt.Errorf("revoking a token: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Ungrantable is the set of actions a token may never carry.
//
// It is NOT simply authz.Protected(), and the difference is worth stating
// because a reader will reach for Protected() and be wrong in one specific way.
//
// Protected() marks actions that normally require a durable human approval
// before execution. approval.decide is deliberately not among them — deciding
// an approval cannot itself require an approval without becoming circular, and
// internal/authz is right to leave it out.
//
// But approval.decide is exactly the action a token must never hold, and for a
// different reason: a personal token IS the person, so an agent holding its
// owner's token could approve its own dispatch, which defeats ADR-0009's digest
// binding entirely. "Requires an approval" and "may be delegated to a
// credential a program holds" are two different questions, and this set answers
// the second.
//
// The list is mirrored by a CHECK constraint in 0038_api_tokens.sql, which is
// the authority. This exists so a refusal names the offending action instead of
// surfacing a constraint violation.
var Ungrantable = map[authz.Action]struct{}{
	authz.ApprovalDecide:                   {},
	authz.PullRequestMerge:                 {},
	authz.DeploymentExecute:                {},
	authz.SecretManage:                     {},
	authz.PolicyManage:                     {},
	authz.MarketingPublish:                 {},
	authz.KnowledgeClassificationDowngrade: {},
	// A reviewer is always a named human (plan section 14.4, and the reason
	// knowledge_reviews.reviewer_user_id references users). A token is held by
	// a program, and a program accepting its own extractor's output into
	// company memory is the loop this workflow exists to break.
	authz.KnowledgeReview: {},
}

// GrantableScopes returns the actions a token may actually hold, sorted. It is
// the answer to "then what may I ask for", which is the only useful thing to
// say to a caller who named a scope that does not exist.
func GrantableScopes() []string {
	var out []string
	for _, a := range authz.AllActions() {
		if _, no := Ungrantable[a]; no || a.Protected() {
			continue
		}
		out = append(out, string(a))
	}
	return out
}

// ValidateScopes refuses any action a token may never carry.
func ValidateScopes(scopes []string) error {
	for _, s := range scopes {
		a := authz.Action(s)
		if !a.Known() {
			return fmt.Errorf("%w: %s", ErrUnknownScope, s)
		}
		// Protected() is consulted as well as the explicit set, so an action
		// that becomes protected in internal/authz later is refused here
		// without anyone remembering to add it below.
		if _, no := Ungrantable[a]; no || a.Protected() {
			return fmt.Errorf("%w: %s", ErrProtectedScope, s)
		}
	}
	return nil
}

// generate returns a new secret and its digest.
func generate() (string, [32]byte, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", [32]byte{}, fmt.Errorf("reading randomness: %w", err)
	}
	secret := Prefix + base64.RawURLEncoding.EncodeToString(buf)
	return secret, sha256.Sum256([]byte(secret)), nil
}

// digestOf returns the digest of a presented secret, and whether it is even
// shaped like one of ours. Rejecting on shape first means an unrelated bearer
// token costs a string comparison rather than a database round trip.
func digestOf(secret string) ([32]byte, bool) {
	if !strings.HasPrefix(secret, Prefix) || len(secret) != len(Prefix)+base64.RawURLEncoding.EncodedLen(secretBytes) {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(secret)), true
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// pgTextArray renders a Go slice as a PostgreSQL text[] literal. The driver
// here has no array support, and the values are a closed set of action names
// validated above, so the escaping needed is quoting and backslashes.
func pgTextArray(vs []string) string {
	if len(vs) == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, v := range vs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// parseTextArray reads PostgreSQL's text[] output form.
func parseTextArray(s string) []string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return []string{}
	}
	s = s[1 : len(s)-1]
	if s == "" {
		return []string{}
	}
	var out []string
	var cur strings.Builder
	inQuotes, escaped := false, false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			inQuotes = !inQuotes
		case r == ',' && !inQuotes:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	out = append(out, cur.String())
	return out
}
