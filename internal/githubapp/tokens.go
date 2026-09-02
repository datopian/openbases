// Package githubapp authenticates as a GitHub App and mints short-lived,
// repository-scoped installation tokens.
//
// The plan forbids a long-lived personal access token (ADR-0007): one would be
// broadly scoped, hard to attribute, and catastrophic if it reached a log. An
// installation token expires within the hour and reaches only the repositories
// of the job that needed it.
package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Client authenticates as the App and mints installation tokens.
type Client struct {
	AppID          string
	InstallationID string
	// PrivateKeyPEM is the App's signing key. It is held in memory rather than
	// re-read per call so the path cannot be swapped underneath a running
	// process, and it never appears in a log line.
	PrivateKeyPEM []byte

	HTTPClient *http.Client
	BaseURL    string
	Now        func() time.Time

	mu     sync.Mutex
	cached *InstallationToken
}

// ErrNotInstalled reports a repository the App installation does not cover.
//
// Distinct from a transient error on purpose: a repository in another
// organisation will fail identically forever, and treating that as a failure
// makes every scheduled run red.
var ErrNotInstalled = errors.New("repository is not covered by this installation")

// ErrOwnerQualified reports a repository named "owner/name" where the API
// wants "name".
//
// Its own error because GitHub answers both with the same 422, and the two
// need opposite responses: one is a grant somebody has to make, the other is a
// caller passing the wrong string. Publication spent a deploy believing the
// installation was missing a repository the installation already had.
var ErrOwnerQualified = errors.New("a scoped token takes bare repository names, not owner/name")

// InstallationToken is a short-lived credential scoped to an installation.
type InstallationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	// Repositories, when the token was minted for a subset.
	Repositories []string `json:"-"`
}

// Expired reports whether the token is spent, with a margin so a token does not
// die mid-request.
func (t *InstallationToken) Expired(now time.Time) bool {
	return t == nil || now.After(t.ExpiresAt.Add(-2*time.Minute))
}

// LoadPrivateKey reads the App key from disk.
//
// It refuses a world- or group-readable file. The key can mint tokens for every
// installed repository, so its permissions are part of the security model, not
// a filesystem detail.
func LoadPrivateKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return nil, fmt.Errorf(
			"private key %s has mode %04o; it must not be readable by group or other", path, mode)
	}
	return os.ReadFile(path)
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	return "https://api.github.com"
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// appJWT signs a short-lived assertion proving we hold the App's key.
//
// GitHub rejects an expiry more than ten minutes ahead, and clock skew on the
// issued-at is a common cause of confusing 401s, so iat is backdated a minute.
func (c *Client) appJWT() (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM(c.PrivateKeyPEM)
	if err != nil {
		return "", fmt.Errorf("parsing App private key: %w", err)
	}
	now := c.now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
		Issuer:    c.AppID,
	})
	return tok.SignedString(key)
}

// InstallationToken returns a valid token, minting a new one when needed.
//
// Repositories, when non-empty, narrows the token to those names. A job that
// touches one repository should not hold a credential for nine.
func (c *Client) InstallationToken(ctx context.Context, repositories ...string) (*InstallationToken, error) {
	// Caught here rather than at GitHub, which answers with the same 422 it
	// uses for a repository the installation does not cover.
	for _, r := range repositories {
		if strings.Contains(r, "/") {
			return nil, fmt.Errorf("%w: %q", ErrOwnerQualified, r)
		}
	}

	// A narrowed token is never cached: the cache is keyed by nothing, so
	// returning it for a different repository set would silently widen scope.
	if len(repositories) == 0 {
		c.mu.Lock()
		if !c.cached.Expired(c.now()) {
			tok := c.cached
			c.mu.Unlock()
			return tok, nil
		}
		c.mu.Unlock()
	}

	assertion, err := c.appJWT()
	if err != nil {
		return nil, err
	}

	body := []byte("{}")
	if len(repositories) > 0 {
		body, err = json.Marshal(map[string]any{"repositories": repositories})
		if err != nil {
			return nil, err
		}
	}

	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", c.baseURL(), c.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+assertion)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusCreated {
		// The body is not included: on some failures GitHub echoes request
		// detail, and this error is likely to be logged.
		// 422 on a scoped mint means the installation does not cover the
		// repository — it lives in an organisation the App was never installed
		// on. That is a configuration fact, not a transient failure, and the
		// caller needs to tell them apart: retrying forever and alerting every
		// time trains everyone to ignore the alert.
		if resp.StatusCode == http.StatusUnprocessableEntity {
			return nil, fmt.Errorf("%w: %v", ErrNotInstalled, repositories)
		}
		return nil, fmt.Errorf("minting installation token: GitHub returned %d", resp.StatusCode)
	}

	var tok InstallationToken
	if err := json.Unmarshal(payload, &tok); err != nil {
		return nil, fmt.Errorf("decoding token response: %w", err)
	}
	if tok.Token == "" {
		return nil, fmt.Errorf("GitHub returned an empty token")
	}
	tok.Repositories = repositories

	if len(repositories) == 0 {
		c.mu.Lock()
		c.cached = &tok
		c.mu.Unlock()
	}
	return &tok, nil
}

// String hides the token. Without this a struct printed into a log or an error
// would leak a live credential, which is the exact failure ADR-0007 exists to
// bound.
func (t InstallationToken) String() string {
	return fmt.Sprintf("InstallationToken{expires:%s repositories:%v}",
		t.ExpiresAt.Format(time.RFC3339), t.Repositories)
}

// Get performs an authenticated GET against the GitHub API.
//
// It exists so callers do not rebuild the base URL, the API version header, or
// the client timeout. Those defaults being in one place is what keeps an
// unattended path (reconciliation) behaving like the request path.
//
// The caller closes the body.
func (c *Client) Get(ctx context.Context, tok *InstallationToken, path string) (*http.Response, error) {
	if tok == nil || tok.Token == "" {
		return nil, errors.New("no installation token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return c.httpClient().Do(req)
}
