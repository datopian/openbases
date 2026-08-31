package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ServiceAccount is the fields of a Google service-account key file we use.
type ServiceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	ClientID    string `json:"client_id"`
	TokenURI    string `json:"token_uri"`
}

// LoadServiceAccount reads a key file.
//
// The path normally comes from GOOGLE_APPLICATION_CREDENTIALS, which
// scripts/with_secrets.sh materialises from SOPS into a file that is removed
// when the shell exits. The key is never in the repository and never in the
// environment as a value.
func LoadServiceAccount(path string) (ServiceAccount, error) {
	var sa ServiceAccount
	raw, err := os.ReadFile(path)
	if err != nil {
		return sa, fmt.Errorf("reading the service-account key: %w", err)
	}
	if err := json.Unmarshal(raw, &sa); err != nil {
		// Named explicitly because the failure we actually hit was a key
		// materialised as single-quoted YAML rather than JSON, and
		// "invalid character '\''" told nobody anything.
		return sa, fmt.Errorf("the service-account key at %s is not JSON (a SOPS value "+
			"extracted without --extract comes out quoted): %w", path, err)
	}
	if sa.Type != "service_account" {
		return sa, fmt.Errorf("expected a service_account key, got %q", sa.Type)
	}
	if sa.PrivateKey == "" || sa.ClientEmail == "" {
		return sa, fmt.Errorf("the service-account key is missing private_key or client_email")
	}
	return sa, nil
}

// DelegatedTokens mints access tokens for a Workspace user via domain-wide
// delegation.
//
// Hand-rolled against a dependency the repository already has rather than
// pulling in Google's client libraries for one grant type. The whole exchange
// is: sign a JWT whose `sub` is the user we are impersonating, POST it, get an
// access token.
//
// The delegation itself is granted in the Workspace admin console against the
// service account's oauth2ClientId, so it survives key rotation — the key can
// be replaced without touching it.
type DelegatedTokens struct {
	Account ServiceAccount
	// Subject is the Workspace user to impersonate. Domain-wide delegation
	// cannot mint a token for the service account itself: Drive and Meet
	// resources belong to users, and a service account is not a user in the
	// domain.
	Subject string
	Scopes  []string
	HTTP    *http.Client
	Now     func() time.Time

	mu     sync.Mutex
	cached string
	expiry time.Time
}

// Google's assertion flow. The audience is the token endpoint itself.
const googleTokenURI = "https://oauth2.googleapis.com/token"

// Token returns a cached token or mints one.
//
// Cached with a minute of headroom: a reconciliation pass makes several calls
// and each one minting its own token turns a 4-call pass into 4 extra
// round-trips, while a token that expires mid-pass fails a call that had
// nothing wrong with it.
func (d *DelegatedTokens) Token(ctx context.Context) (string, error) {
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cached != "" && now().Add(time.Minute).Before(d.expiry) {
		return d.cached, nil
	}
	if d.Subject == "" {
		return "", fmt.Errorf("domain-wide delegation needs a Workspace user to impersonate; " +
			"a service account cannot hold Drive or Meet resources of its own")
	}
	if len(d.Scopes) == 0 {
		return "", fmt.Errorf("no scopes requested")
	}

	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(d.Account.PrivateKey))
	if err != nil {
		return "", fmt.Errorf("parsing the service-account private key: %w", err)
	}
	issued := now().Add(-30 * time.Second)
	claims := jwt.MapClaims{
		"iss":   d.Account.ClientEmail,
		"sub":   d.Subject,
		"scope": strings.Join(d.Scopes, " "),
		"aud":   d.tokenURI(),
		"iat":   issued.Unix(),
		"exp":   issued.Add(10 * time.Minute).Unix(),
	}
	assertion, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("signing the assertion: %w", err)
	}

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.tokenURI(),
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "workgraph-workspace/1")

	c := d.HTTP
	if c == nil {
		c = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("reading the token response (HTTP %d): %w", resp.StatusCode, err)
	}
	if body.Error != "" || body.AccessToken == "" {
		hint := ""
		if body.Error == "unauthorized_client" {
			// The one failure everyone hits. Worth naming, because the message
			// Google returns does not mention delegation at all.
			hint = " — the scopes are probably not granted to client ID " +
				d.Account.ClientID + " in the Workspace admin console, " +
				"or one granted scope differs by a character"
		}
		if body.Error == "invalid_grant" {
			hint = " — check that " + d.Subject + " is a real user in the domain"
		}
		return "", fmt.Errorf("google refused the assertion: %s: %s%s", body.Error, body.Description, hint)
	}
	d.cached = body.AccessToken
	d.expiry = now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return d.cached, nil
}

func (d *DelegatedTokens) tokenURI() string {
	if d.Account.TokenURI != "" {
		return d.Account.TokenURI
	}
	return googleTokenURI
}

// Scopes the reconciler needs. Read-only, and only these two: the subscription
// API requires the scope of the data it will send events about, so Drive
// subscriptions need Drive read and Meet subscriptions need Meet read. Nothing
// here writes to either.
var (
	ScopeDriveReadonly = "https://www.googleapis.com/auth/drive.readonly"
	ScopeMeetReadonly  = "https://www.googleapis.com/auth/meetings.space.readonly"
)
