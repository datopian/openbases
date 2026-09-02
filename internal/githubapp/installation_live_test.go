package githubapp

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// The knowledge repository has to be inside the App's installation, or WP-H4
// proposes nothing.
//
// This is a live check that skips without credentials, like the token one. It
// exists because the failure it catches is invisible from inside the code: the
// App holds contents:write and pull_requests:write, the repository selection is
// "selected", and a repository outside that selection returns 404 on every
// write with no hint that access is the problem. Publication was written,
// tested and deployed against a repository the App could not reach.
func TestLiveInstallationCoversTheKnowledgeRepository(t *testing.T) {
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

	// The repository the deployed default names. Kept here rather than read
	// from the Ansible defaults: this asserts the thing the code was built
	// against, and a check that reads its expectation from the same place as
	// the deployment cannot disagree with it.
	const want = "datopian/company-workgraph"

	c := &Client{AppID: appID, InstallationID: installID, PrivateKeyPEM: key}
	tok, err := c.InstallationToken(context.Background())
	if err != nil {
		t.Fatalf("minting an installation token: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://api.github.com/installation/repositories?per_page=100", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("listing the installation's repositories: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing the installation's repositories: %s", resp.Status)
	}

	var body struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	var names []string
	for _, r := range body.Repositories {
		if r.FullName == want {
			return
		}
		names = append(names, r.FullName)
	}
	t.Fatalf("the installation does not include %s; it covers %v.\n"+
		"Add it under the organisation's GitHub Apps settings, or knowledge records "+
		"will never be proposed.", want, names)
}
