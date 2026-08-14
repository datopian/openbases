package githubapp

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Exercises the real GitHub App when credentials are present, and skips
// otherwise so CI stays hermetic. Deliberately part of the suite rather than a
// throwaway script: a change to token minting should be checkable against the
// real API by anyone holding the credentials.
func TestLiveInstallationToken(t *testing.T) {
	appID := os.Getenv("GITHUB_APP_ID")
	installID := os.Getenv("GITHUB_APP_INSTALLATION_ID")
	keyPath := os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH")
	if appID == "" || installID == "" || keyPath == "" {
		t.Skip("GitHub App credentials not set; skipping the live check")
	}
	if strings.HasPrefix(keyPath, "~") {
		home, _ := os.UserHomeDir()
		keyPath = home + strings.TrimPrefix(keyPath, "~")
	}

	key, err := LoadPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("loading the App key: %v", err)
	}

	c := &Client{AppID: appID, InstallationID: installID, PrivateKeyPEM: key}

	full, err := c.InstallationToken(context.Background())
	if err != nil {
		t.Fatalf("minting an installation token: %v", err)
	}
	if full.Token == "" {
		t.Fatal("GitHub returned an empty token")
	}
	t.Logf("unscoped: %s", full)

	// A token narrowed to one repository must be a genuinely different
	// credential, not the cached broad one handed back.
	scoped, err := c.InstallationToken(context.Background(), "nged")
	if err != nil {
		t.Fatalf("minting a scoped token: %v", err)
	}
	if scoped.Token == full.Token {
		t.Error("a repository-scoped token must not be the broad cached token")
	}
	t.Logf("scoped:   %s", scoped)
}
