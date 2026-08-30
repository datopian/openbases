package workspace

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

// GoogleIssuer is the issuer Google puts in a Pub/Sub push OIDC token.
const GoogleIssuer = "https://accounts.google.com"

// GoogleJWKS is where Google publishes the keys those tokens are signed with.
const GoogleJWKS = "https://www.googleapis.com/oauth2/v3/certs"

// PushValidator verifies the OIDC token Pub/Sub attaches to a push delivery
// (ADR-0026).
//
// The Access bypass on this path is a routing decision, not the security
// boundary. This is the boundary: a delivery that does not carry a token Google
// signed, for this audience, is not a delivery.
//
// Modelled on authn.AccessValidator deliberately — same shape, same caching,
// same reasons — because two verifiers that look different invite the reader to
// assume one of them is doing something clever.
type PushValidator struct {
	// Audience is the value configured on the push subscription. Without it a
	// token minted for any other Google-fronted service would validate here,
	// which is the difference between proving a request came from Google and
	// proving it was meant for us.
	Audience string

	// ServiceAccount is the email Pub/Sub signs as. Checked against the token's
	// email claim, so a token from a different service account in the same
	// project is refused.
	ServiceAccount string

	HTTPClient *http.Client
	Now        func() time.Time

	// testJWKS overrides the key endpoint. Unexported and set only by tests in
	// this package: pointing key verification at another host is not something
	// a deployment should be able to do by accident or by configuration.
	testJWKS string

	mu        sync.RWMutex
	keys      *jose.JSONWebKeySet
	fetchedAt time.Time
}

// keyTTL bounds how long a fetched key set is reused. Google rotates these and
// publishes a Cache-Control; an hour is short enough to follow a rotation and
// long enough that a delivery burst is not a burst of key fetches.
const keyTTL = time.Hour

type pushClaims struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

// Verify checks the Authorization header of a push request and returns nil when
// the delivery is genuinely from our Pub/Sub subscription.
func (v *PushValidator) Verify(ctx context.Context, authorization string) error {
	if v.Audience == "" {
		// Refused rather than skipped. An empty audience would accept any
		// Google-signed token, which is a worse position than having no check
		// at all because it looks like one.
		return fmt.Errorf("no push audience configured; every Google-signed token would be accepted")
	}
	raw := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	if raw == "" || raw == authorization && strings.Contains(authorization, " ") {
		return fmt.Errorf("no bearer token on the request")
	}

	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return fmt.Errorf("token is not a signed JWT: %w", err)
	}

	keys, err := v.keySet(ctx)
	if err != nil {
		return fmt.Errorf("fetching Google's signing keys: %w", err)
	}

	var std jwt.Claims
	var extra pushClaims
	if err := tok.Claims(keys, &std, &extra); err != nil {
		// One retry with fresh keys, and only on a signature failure: Google
		// rotates, and a cached set can be a few minutes stale. Retrying every
		// error would turn a malformed token into two key fetches.
		fresh, ferr := v.refresh(ctx)
		if ferr != nil {
			return fmt.Errorf("signature check failed and keys could not be refreshed: %w", err)
		}
		if err = tok.Claims(fresh, &std, &extra); err != nil {
			return fmt.Errorf("signature is not valid for any current Google key: %w", err)
		}
	}

	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	if err := std.ValidateWithLeeway(jwt.Expected{
		Issuer:      GoogleIssuer,
		AnyAudience: jwt.Audience{v.Audience},
		Time:        now(),
	}, time.Minute); err != nil {
		return fmt.Errorf("token claims are not acceptable: %w", err)
	}

	if v.ServiceAccount != "" && !strings.EqualFold(extra.Email, v.ServiceAccount) {
		// A token signed by Google for a DIFFERENT service account is a valid
		// Google token and not one of ours.
		return fmt.Errorf("token was issued to %q, not %q", extra.Email, v.ServiceAccount)
	}
	return nil
}

func (v *PushValidator) keySet(ctx context.Context) (*jose.JSONWebKeySet, error) {
	v.mu.RLock()
	keys, at := v.keys, v.fetchedAt
	v.mu.RUnlock()
	if keys != nil && time.Since(at) < keyTTL {
		return keys, nil
	}
	return v.refresh(ctx)
}

func (v *PushValidator) refresh(ctx context.Context) (*jose.JSONWebKeySet, error) {
	c := v.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("key endpoint returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("key endpoint returned something that is not a JWKS: %w", err)
	}
	if len(set.Keys) == 0 {
		// Caching an empty set would fail every delivery for an hour.
		return nil, fmt.Errorf("key endpoint returned no keys")
	}
	v.mu.Lock()
	v.keys, v.fetchedAt = &set, time.Now()
	v.mu.Unlock()
	return &set, nil
}

// jwksURL is overridable only in tests; there is no configuration for it,
// because pointing key verification at another host is not a thing a
// deployment should be able to do by accident.
func (v *PushValidator) jwksURL() string {
	if v.testJWKS != "" {
		return v.testJWKS
	}
	return GoogleJWKS
}
