package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// The App key can mint tokens for every installed repository, so its file
// permissions are part of the security model rather than a filesystem detail.
func TestLoadPrivateKeyRefusesLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(path, testKeyPEM(t), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadPrivateKey(path); err == nil {
		t.Fatal("a group- or world-readable App key must be refused")
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrivateKey(path); err != nil {
		t.Fatalf("a 0600 key should load: %v", err)
	}
}

// A token printed into a log or an error must not leak the credential.
func TestTokenNeverPrintsItsSecret(t *testing.T) {
	tok := InstallationToken{
		Token:        "ghs_averysecretinstallationtoken",
		ExpiresAt:    time.Now().Add(time.Hour),
		Repositories: []string{"datopian/nged"},
	}
	for _, rendered := range []string{
		tok.String(),
		fmt.Sprintf("%v", tok),
		fmt.Sprintf("%s", tok),
	} {
		if strings.Contains(rendered, "ghs_") {
			t.Fatalf("rendering a token leaked the credential: %s", rendered)
		}
	}
}

func TestExpiryHasMargin(t *testing.T) {
	now := time.Now()
	// Still inside its lifetime, but close enough that it could die mid-request.
	tok := &InstallationToken{ExpiresAt: now.Add(time.Minute)}
	if !tok.Expired(now) {
		t.Error("a token about to expire must be treated as spent")
	}
	if !(*InstallationToken)(nil).Expired(now) {
		t.Error("a nil token must count as expired")
	}
	if (&InstallationToken{ExpiresAt: now.Add(time.Hour)}).Expired(now) {
		t.Error("a fresh token must not be treated as expired")
	}
}

func TestMintsAndCachesUnscopedTokens(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_token",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	c := &Client{AppID: "1", InstallationID: "2", PrivateKeyPEM: testKeyPEM(t), BaseURL: srv.URL}

	for i := 0; i < 3; i++ {
		if _, err := c.InstallationToken(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Errorf("expected the token to be cached, minted %d times", calls)
	}
}

// A narrowed token must never be served from the cache: doing so would hand a
// caller a credential for repositories it did not ask for.
func TestScopedTokensAreNotCached(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		bodies = append(bodies, string(buf))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_scoped",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	c := &Client{AppID: "1", InstallationID: "2", PrivateKeyPEM: testKeyPEM(t), BaseURL: srv.URL}

	if _, err := c.InstallationToken(context.Background(), "nged"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InstallationToken(context.Background(), "portaljs"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("each scoped request must mint its own token, got %d", len(bodies))
	}
	if !strings.Contains(bodies[0], "nged") || !strings.Contains(bodies[1], "portaljs") {
		t.Errorf("the requested repositories were not sent: %v", bodies)
	}
}

// GitHub sometimes echoes request detail on failure, and this error is likely
// to be logged.
func TestMintFailureDoesNotEchoTheResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials","secret_echo":"ghs_leaked"}`))
	}))
	defer srv.Close()

	c := &Client{AppID: "1", InstallationID: "2", PrivateKeyPEM: testKeyPEM(t), BaseURL: srv.URL}
	_, err := c.InstallationToken(context.Background())
	if err == nil {
		t.Fatal("a 401 must be an error")
	}
	if strings.Contains(err.Error(), "ghs_leaked") || strings.Contains(err.Error(), "secret_echo") {
		t.Errorf("the error echoed the response body: %v", err)
	}
}

// GitHub answers an owner-qualified repository name with the same 422 it uses
// for a repository the installation does not cover, so the two are impossible
// to tell apart from the response. This catches the caller's mistake before
// the request, and names it.
func TestScopedTokensRefuseOwnerQualifiedNames(t *testing.T) {
	c := &Client{AppID: "1", InstallationID: "2", PrivateKeyPEM: testKeyPEM(t)}
	_, err := c.InstallationToken(context.Background(), "datopian/company-workgraph")
	if !errors.Is(err, ErrOwnerQualified) {
		t.Fatalf("error = %v, want ErrOwnerQualified", err)
	}
	if !strings.Contains(err.Error(), "datopian/company-workgraph") {
		t.Fatalf("the error does not name the offending value: %v", err)
	}
}
