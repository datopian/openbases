package authn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	testTeam = "datopian.cloudflareaccess.com"
	testAud  = "aud-for-the-web-application"
)

type harness struct {
	key       *rsa.PrivateKey
	otherKey  *rsa.PrivateKey
	server    *httptest.Server
	validator *AccessValidator
	now       time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	h := &harness{key: key, otherKey: other, now: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}

	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       key.Public(),
			KeyID:     "test-key",
			Algorithm: string(jose.RS256),
			Use:       "sig",
		}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(h.server.Close)

	h.validator = &AccessValidator{
		TeamDomain: testTeam,
		Audience:   testAud,
		Now:        func() time.Time { return h.now },
		// Redirect key fetches to the test server regardless of the URL built
		// from TeamDomain, so the issuer check is still exercised for real.
		HTTPClient: &http.Client{Transport: rewriteTo(h.server.URL)},
	}
	return h
}

type rewrite struct{ base string }

func rewriteTo(base string) http.RoundTripper { return rewrite{base} }

func (rt rewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	u := *r.URL
	u.Scheme = "http"
	u.Host = rt.base[len("http://"):]
	r2 := r.Clone(r.Context())
	r2.URL = &u
	return http.DefaultTransport.RoundTrip(r2)
}

// sign mints a token. Every parameter is variable so each test can break
// exactly one property.
func (h *harness) sign(t *testing.T, key *rsa.PrivateKey, alg jose.SignatureAlgorithm, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: alg, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"),
	)
	if err != nil {
		t.Fatal(err)
	}
	s, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (h *harness) validClaims() map[string]any {
	return map[string]any{
		"iss":   "https://" + testTeam,
		"aud":   []string{testAud},
		"sub":   "user-subject-1",
		"email": "anuar.ustayev@datopian.com",
		"exp":   h.now.Add(time.Hour).Unix(),
		"iat":   h.now.Add(-time.Minute).Unix(),
		"nbf":   h.now.Add(-time.Minute).Unix(),
	}
}

func (h *harness) request(token string) *http.Request {
	r := httptest.NewRequest("GET", "/v1/me", nil)
	r.Header.Set("Cf-Access-Jwt-Assertion", token)
	return r
}

func TestValidTokenAuthenticates(t *testing.T) {
	h := newHarness(t)
	id, err := h.validator.Authenticate(context.Background(), h.request(h.sign(t, h.key, jose.RS256, h.validClaims())))
	if err != nil {
		t.Fatalf("a valid token must authenticate: %v", err)
	}
	if id.Subject != "user-subject-1" || id.Email != "anuar.ustayev@datopian.com" {
		t.Errorf("unexpected identity: %+v", id)
	}
	if id.IsService {
		t.Error("a user token must not be treated as a service token")
	}
}

// The single most important test in this package. A header alone must never
// authenticate: anything that reaches the origin can set one.
func TestForgedHeaderWithoutTokenIsRejected(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest("GET", "/v1/me", nil)
	r.Header.Set("Cf-Access-Authenticated-User-Email", "attacker@example.com")
	r.Header.Set("Cf-Access-Authenticated-User-Id", "somebody")

	if _, err := h.validator.Authenticate(context.Background(), r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("identity headers alone must never authenticate, got %v", err)
	}
}

func TestTokenSignedByAnotherKeyIsRejected(t *testing.T) {
	h := newHarness(t)
	tok := h.sign(t, h.otherKey, jose.RS256, h.validClaims())
	if _, err := h.validator.Authenticate(context.Background(), h.request(tok)); err == nil {
		t.Fatal("a token signed by an unknown key must be rejected")
	}
}

// Algorithm confusion: a token declaring HS256 and signed with the public key
// as an HMAC secret verifies if the algorithm is taken from the token header.
func TestAlgorithmConfusionIsRejected(t *testing.T) {
	h := newHarness(t)
	// The real attack uses the RSA public key's own bytes as the HMAC secret,
	// which is what makes it verify when the algorithm is taken from the token
	// header. Marshal the actual public key rather than a short literal, both
	// to model the attack and because HS256 requires at least 32 bytes.
	pub, err := x509.MarshalPKIXPublicKey(h.key.Public())
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: pub},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"),
	)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(signer).Claims(h.validClaims()).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.validator.Authenticate(context.Background(), h.request(tok)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("an HS256 token must be rejected when only RS256 is allowed, got %v", err)
	}
}

func TestExpiredTokenIsRejected(t *testing.T) {
	h := newHarness(t)
	c := h.validClaims()
	c["exp"] = h.now.Add(-time.Second).Unix()
	if _, err := h.validator.Authenticate(context.Background(), h.request(h.sign(t, h.key, jose.RS256, c))); err == nil {
		t.Fatal("an expired token must be rejected")
	}
}

func TestNotYetValidTokenIsRejected(t *testing.T) {
	h := newHarness(t)
	c := h.validClaims()
	c["nbf"] = h.now.Add(time.Hour).Unix()
	if _, err := h.validator.Authenticate(context.Background(), h.request(h.sign(t, h.key, jose.RS256, c))); err == nil {
		t.Fatal("a token that is not yet valid must be rejected")
	}
}

// A token minted for the SSH application must not open the web application,
// even though both are signed by the same team key.
func TestTokenForAnotherApplicationIsRejected(t *testing.T) {
	h := newHarness(t)
	c := h.validClaims()
	c["aud"] = []string{"aud-for-the-ssh-application"}
	if _, err := h.validator.Authenticate(context.Background(), h.request(h.sign(t, h.key, jose.RS256, c))); err == nil {
		t.Fatal("a token for a different Access application must be rejected")
	}
}

// A token from a different Cloudflare team, signed by that team's key, must not
// be accepted just because the audience happens to match.
func TestTokenFromAnotherTeamIsRejected(t *testing.T) {
	h := newHarness(t)
	c := h.validClaims()
	c["iss"] = "https://someone-else.cloudflareaccess.com"
	if _, err := h.validator.Authenticate(context.Background(), h.request(h.sign(t, h.key, jose.RS256, c))); err == nil {
		t.Fatal("a token issued by another team must be rejected")
	}
}

func TestTokenWithoutSubjectIsRejected(t *testing.T) {
	h := newHarness(t)
	c := h.validClaims()
	delete(c, "sub")
	if _, err := h.validator.Authenticate(context.Background(), h.request(h.sign(t, h.key, jose.RS256, c))); err == nil {
		t.Fatal("a token with no subject cannot identify anyone")
	}
}

func TestUserTokenWithoutEmailIsRejected(t *testing.T) {
	h := newHarness(t)
	c := h.validClaims()
	delete(c, "email")
	if _, err := h.validator.Authenticate(context.Background(), h.request(h.sign(t, h.key, jose.RS256, c))); err == nil {
		t.Fatal("a user token with no email must be rejected rather than becoming an anonymous session")
	}
}

// A service token is a machine. It must be distinguishable, because it cannot
// review knowledge or satisfy an approval.
func TestServiceTokenIsMarkedAsService(t *testing.T) {
	h := newHarness(t)
	c := h.validClaims()
	delete(c, "email")
	c["type"] = "non_identity"
	c["common_name"] = "workgraph-staging-config-management"

	id, err := h.validator.Authenticate(context.Background(), h.request(h.sign(t, h.key, jose.RS256, c)))
	if err != nil {
		t.Fatalf("a valid service token should authenticate: %v", err)
	}
	if !id.IsService {
		t.Error("a service token must be marked as a service")
	}
	if id.Email != "" {
		t.Error("a service token must carry no email; it is not a person")
	}
	if id.ServiceName == "" {
		t.Error("a service token must be attributable to a named token")
	}
}

func TestTokenFromCookieIsAccepted(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest("GET", "/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: "CF_Authorization", Value: h.sign(t, h.key, jose.RS256, h.validClaims())})
	if _, err := h.validator.Authenticate(context.Background(), r); err != nil {
		t.Fatalf("browsers present the token as a cookie: %v", err)
	}
}

func TestGarbageTokenIsRejected(t *testing.T) {
	h := newHarness(t)
	for _, tok := range []string{"not-a-jwt", "a.b.c", "", "eyJhbGciOiJub25lIn0..", "Bearer x"} {
		if _, err := h.validator.Authenticate(context.Background(), h.request(tok)); err == nil {
			t.Errorf("malformed token %q must be rejected", tok)
		}
	}
}

// An audience bound to a path must be accepted there and NOWHERE else.
//
// This is the property that keeps the cell token from becoming a general API
// credential: the cell's Access application has its own AUD, and if that AUD
// validated everywhere, a token that exists only to fetch a git credential
// could read every project's work with no human identity attached.
func TestPathBoundAudienceIsNotAcceptedElsewhere(t *testing.T) {
	v := &AccessValidator{
		TeamDomain: "datopian.cloudflareaccess.com",
		Audience:   "app-aud",
		PathAudiences: map[string]string{
			"/v1/integrations/github/installation-token": "cell-aud",
		},
	}

	onPath := v.audiencesFor("/v1/integrations/github/installation-token")
	if len(onPath) != 2 || onPath[0] != "app-aud" || onPath[1] != "cell-aud" {
		t.Fatalf("the bound path must accept both audiences, got %v", onPath)
	}

	for _, path := range []string{
		"/v1/projects",
		"/v1/me",
		// A prefix of the bound path, and an extension of it. Matching by
		// prefix would accept both, which is why matching is exact.
		"/v1/integrations/github",
		"/v1/integrations/github/installation-token/../projects",
		"/v1/integrations/github/installation-token/extra",
	} {
		got := v.audiencesFor(path)
		if len(got) != 1 || got[0] != "app-aud" {
			t.Errorf("%s: the cell audience leaked outside its path: %v", path, got)
		}
	}
}

// With no path audiences configured, behaviour is unchanged.
func TestAudiencesWithoutPathBindings(t *testing.T) {
	v := &AccessValidator{Audience: "app-aud"}
	got := v.audiencesFor("/v1/integrations/github/installation-token")
	if len(got) != 1 || got[0] != "app-aud" {
		t.Fatalf("expected only the application audience, got %v", got)
	}
}

// Cloudflare issues service-token JWTs with an EMPTY sub and the token's name
// in common_name.
//
// Requiring a subject before the service-token branch rejected every one of
// them with "token has no subject" while the token was entirely valid — and the
// message pointed at the wrong thing, which is what made it slow to find.
func TestServiceTokenWithNoSubjectIsAccepted(t *testing.T) {
	h := newHarness(t)

	claims := h.validClaims()
	delete(claims, "sub")
	delete(claims, "email")
	claims["type"] = "non_identity"
	claims["common_name"] = "workgraph-staging-cells"

	id, err := h.validator.Authenticate(context.Background(),
		h.request(h.sign(t, h.key, jose.RS256, claims)))
	if err != nil {
		t.Fatalf("a valid service token was refused: %v", err)
	}
	if !id.IsService {
		t.Error("not marked as a service caller")
	}
	if id.ServiceName != "workgraph-staging-cells" {
		t.Errorf("service name lost: %q", id.ServiceName)
	}
	// The subject falls back to the common name so the audit trail names the
	// token rather than being blank.
	if id.Subject != "workgraph-staging-cells" {
		t.Errorf("subject should fall back to the common name, got %q", id.Subject)
	}
	if id.Email != "" {
		t.Error("a service token must carry no email; a caller could mistake it for a user")
	}
}

// A user token still requires a subject.
func TestUserTokenStillRequiresASubject(t *testing.T) {
	h := newHarness(t)

	claims := h.validClaims()
	delete(claims, "sub")
	claims["email"] = "person@datopian.com"

	if _, err := h.validator.Authenticate(context.Background(),
		h.request(h.sign(t, h.key, jose.RS256, claims))); err == nil {
		t.Fatal("a user token with no subject must be refused")
	}
}
