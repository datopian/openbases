package githubapp

import (
	"context"
	"strings"
	"testing"
	"time"
)

func tok(s string) *InstallationToken {
	return &InstallationToken{Token: s, ExpiresAt: time.Now().Add(time.Hour)}
}

// The clone URL carries a live credential. These assertions are about not
// leaking it, which is the whole risk of embedding a token in a URL.
func TestCloneURLCarriesTheTokenAndNothingElse(t *testing.T) {
	u, err := CloneURL(tok("ghs_secret"), "datopian", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "https://x-access-token:ghs_secret@github.com/datopian/sandbox.git") {
		t.Errorf("unexpected clone URL shape: %s", u)
	}
	if _, err := CloneURL(nil, "o", "r"); err == nil {
		t.Error("a nil token must be refused rather than producing an unauthenticated URL")
	}
	if _, err := CloneURL(&InstallationToken{}, "o", "r"); err == nil {
		t.Error("an empty token must be refused")
	}
}

// Every one of these is a mistake an agent can plausibly make, and each is
// caught before a request is sent so the agent gets a sentence it can act on
// rather than a 422 it has to interpret.
func TestOpenPullRequestRefusesIncompleteRequests(t *testing.T) {
	c := &Client{}
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		req  OpenPullRequestRequest
	}{
		{"no owner", OpenPullRequestRequest{Repo: "r", Head: "h", Base: "main", Title: "t"}},
		{"no repo", OpenPullRequestRequest{Owner: "o", Head: "h", Base: "main", Title: "t"}},
		{"no head", OpenPullRequestRequest{Owner: "o", Repo: "r", Base: "main", Title: "t"}},
		{"no base", OpenPullRequestRequest{Owner: "o", Repo: "r", Head: "h", Title: "t"}},
		{"no title", OpenPullRequestRequest{Owner: "o", Repo: "r", Head: "h", Base: "main", Title: "  "}},
	} {
		if _, err := c.OpenPullRequest(ctx, tok("x"), tc.req); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}

	if _, err := c.OpenPullRequest(ctx, nil, OpenPullRequestRequest{
		Owner: "o", Repo: "r", Head: "h", Base: "main", Title: "t"}); err == nil {
		t.Error("a nil token was accepted")
	}
}

// An agent that forgets to branch would otherwise ask GitHub to merge main into
// main. The error names that specifically, because "422 Unprocessable Entity"
// does not tell an agent what it did wrong.
func TestOpenPullRequestRefusesHeadEqualToBase(t *testing.T) {
	c := &Client{}
	_, err := c.OpenPullRequest(context.Background(), tok("x"), OpenPullRequestRequest{
		Owner: "o", Repo: "r", Head: "main", Base: "main", Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "did not create a branch") {
		t.Fatalf("expected a branch-specific error, got %v", err)
	}
}
