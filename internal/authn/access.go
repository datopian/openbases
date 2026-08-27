package authn

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// AccessValidator validates a Cloudflare Access JWT.
//
// The application validates the token itself rather than trusting request
// headers (ADR-0006). Anything that reaches the origin can set a header; only
// Cloudflare can sign a token with the team's key.
type AccessValidator struct {
	// TeamDomain is the full Zero Trust domain, e.g.
	// datopian.cloudflareaccess.com. It is both the JWKS host and the expected
	// issuer, so a token minted by another team is rejected.
	TeamDomain string

	// Audience is the Access application's AUD tag. Without it, a token issued
	// for a DIFFERENT application in the same account would validate here —
	// which is how the SSH application's token could be replayed against the
	// web application.
	Audience string

	// PathAudiences maps a request path to an ADDITIONAL audience accepted only
	// for that path.
	//
	// Cells authenticate through their own Access application, which has its own
	// AUD, and that token must not become a general-purpose API credential — the
	// point of checking the audience at all is that a token minted for one
	// application does not validate against another. Binding the extra audience
	// to its path keeps that property instead of quietly widening it.
	PathAudiences map[string]string

	// PathPrefixAudiences maps a reserved path PREFIX to an audience accepted
	// anywhere beneath it. Only for namespaces that exist to be one; see
	// audiencesFor.
	PathPrefixAudiences map[string]string

	// HTTPClient fetches the signing keys. Injectable for tests.
	HTTPClient *http.Client

	// Now is the clock, injectable so expiry can be tested without sleeping.
	Now func() time.Time

	mu        sync.RWMutex
	keys      *jose.JSONWebKeySet
	fetchedAt time.Time
}

// keyTTL bounds how long a fetched key set is reused. Cloudflare rotates
// signing keys, and a stale set makes every request fail closed rather than
// open — but refetching on every request would make the identity edge a
// dependency of every single call.
const keyTTL = 15 * time.Minute

// allowedAlgorithms is deliberately a fixed list. Accepting whatever the token
// header declares is the algorithm-confusion vulnerability: a token with
// alg=none, or alg=HS256 signed with the public key as an HMAC secret, would
// otherwise verify.
var allowedAlgorithms = []jose.SignatureAlgorithm{jose.RS256}

// accessClaims are the registered claims plus the Access-specific ones we use.
type accessClaims struct {
	Email string `json:"email"`
	// Type distinguishes a user token from a service token. A service token
	// carries no email, and treating one as a user would create an
	// unattributable session.
	Type       string `json:"type"`
	CommonName string `json:"common_name"`
}

// Authenticate validates the Access JWT on the request and returns the identity.
func (v *AccessValidator) Authenticate(ctx context.Context, r *http.Request) (Identity, error) {
	raw := accessToken(r)
	if raw == "" {
		return Identity{}, fmt.Errorf("%w: no Access token on the request", ErrUnauthenticated)
	}

	tok, err := jwt.ParseSigned(raw, allowedAlgorithms)
	if err != nil {
		// Covers alg=none and any algorithm outside the allow-list.
		return Identity{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}

	keys, err := v.keySet(ctx, false)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: cannot verify signature: %v", ErrUnauthenticated, err)
	}

	var std jwt.Claims
	var claims accessClaims
	if err := tok.Claims(keys, &std, &claims); err != nil {
		// A signing key may have rotated since the cache was filled. Refetch
		// once before rejecting, so a rotation is not an outage.
		keys, ferr := v.keySet(ctx, true)
		if ferr != nil {
			return Identity{}, fmt.Errorf("%w: signature invalid", ErrUnauthenticated)
		}
		if err := tok.Claims(keys, &std, &claims); err != nil {
			return Identity{}, fmt.Errorf("%w: signature invalid", ErrUnauthenticated)
		}
	}

	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}

	// Issuer and audience are both required. Checking expiry alone would accept
	// a validly-signed token minted for another application.
	if err := std.ValidateWithLeeway(jwt.Expected{
		Issuer:      "https://" + v.TeamDomain,
		AnyAudience: v.audiencesFor(r.URL.Path),
		Time:        now,
	}, 0); err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}

	// A service token authenticates a machine, not a person. It is returned
	// with no email so that a caller cannot mistake it for a user, and so audit
	// records attribute it to the token rather than to nobody.
	//
	// Cloudflare issues these with an EMPTY sub and the token's name in
	// common_name, so the subject check below cannot come first: it rejected
	// every service token with "token has no subject" while the token was
	// perfectly valid. The common name is used as the subject instead, which is
	// also what makes the audit trail name the token.
	if claims.Type == "non_identity" || (claims.Email == "" && claims.CommonName != "") {
		if claims.CommonName == "" {
			return Identity{}, fmt.Errorf("%w: service token has no common name", ErrUnauthenticated)
		}
		subject := std.Subject
		if subject == "" {
			subject = claims.CommonName
		}
		return Identity{
			Subject:     subject,
			ServiceName: claims.CommonName,
			IsService:   true,
		}, nil
	}

	// A user token must carry a subject; without one there is nobody to
	// attribute the request to.
	if std.Subject == "" {
		return Identity{}, fmt.Errorf("%w: token has no subject", ErrUnauthenticated)
	}

	if claims.Email == "" {
		return Identity{}, fmt.Errorf("%w: user token carries no email", ErrUnauthenticated)
	}

	return Identity{Subject: std.Subject, Email: claims.Email}, nil
}

// accessToken reads the Access JWT.
//
// Cloudflare presents it as the CF_Authorization cookie and, for non-browser
// clients, the Cf-Access-Jwt-Assertion header. Both are read; neither is
// trusted until the signature is verified, which is the entire point.
func accessToken(r *http.Request) string {
	if h := r.Header.Get("Cf-Access-Jwt-Assertion"); h != "" {
		return h
	}
	if c, err := r.Cookie("CF_Authorization"); err == nil {
		return c.Value
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimPrefix(a, "Bearer ")
	}
	return ""
}

// keySet returns Cloudflare's signing keys, refetching when the cache is stale
// or when force is set.
func (v *AccessValidator) keySet(ctx context.Context, force bool) (*jose.JSONWebKeySet, error) {
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}

	if !force {
		v.mu.RLock()
		cached, at := v.keys, v.fetchedAt
		v.mu.RUnlock()
		if cached != nil && now.Sub(at) < keyTTL {
			return cached, nil
		}
	}

	client := v.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	url := "https://" + v.TeamDomain + "/cdn-cgi/access/certs"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("key endpoint returned %d", resp.StatusCode)
	}

	// Bound the read: the identity edge is a remote dependency, and an
	// unbounded body would let it exhaust memory here.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("malformed key set: %w", err)
	}
	if len(set.Keys) == 0 {
		return nil, fmt.Errorf("key set is empty")
	}

	v.mu.Lock()
	v.keys, v.fetchedAt = &set, now
	v.mu.Unlock()
	return &set, nil
}

var _ Authenticator = (*AccessValidator)(nil)

// audiencesFor returns the audiences acceptable for a request path.
//
// The application audience is always included; a path-bound one is added only
// for its own path. Matching is exact rather than by prefix, because a prefix
// match on "/v1/integrations/github/installation-token" would also accept
// anything appended to it.
func (v *AccessValidator) audiencesFor(path string) jwt.Audience {
	auds := jwt.Audience{v.Audience}
	if extra, ok := v.PathAudiences[path]; ok && extra != "" {
		auds = append(auds, extra)
	}
	// A prefix is accepted only for a namespace that exists to BE one.
	//
	// The rule above is right for the paths it guards: a prefix match on
	// "/v1/integrations/github/installation-token" would also accept anything
	// appended to it. It cannot express "/v1/node/work/{id}/result", whose id
	// varies, and enumerating job ids is not possible.
	//
	// So /v1/node/ is reserved: everything under it is a node endpoint and
	// nothing else is ever mounted there. The prefix is the security boundary,
	// which is only safe because the namespace is dedicated — the human
	// endpoints that CREATE work live under /v1/work, deliberately not here.
	for prefix, extra := range v.PathPrefixAudiences {
		if extra != "" && strings.HasPrefix(path, prefix) {
			auds = append(auds, extra)
		}
	}
	return auds
}
