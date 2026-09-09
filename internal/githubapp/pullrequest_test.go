package githubapp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

// A reused pull request's description is rewritten to match its diff.
//
// A re-dispatched bead pushes to the same branch and reuses its pull request,
// deliberately -- work re-dispatched must not multiply pull requests. But the
// description was written by the FIRST landing and never touched again, so it
// went stale the moment a later run changed the diff. datopian/msf#1 listed
// `node_modules/` among its files for hours after the branch stopped
// containing any, and a reviewer cannot tell which half is out of date.
func TestUpdatePullRequestRewritesTitleAndBody(t *testing.T) {
	var method, path, got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"number":1}`)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	tok := &InstallationToken{Token: "tok"}

	if err := c.UpdatePullRequest(context.Background(), tok, "datopian", "msf", 1,
		"Scaffold a portal (sa-7dc)", "the current description"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH: anything else would open a second "+
			"pull request rather than correct this one", method)
	}
	if path != "/repos/datopian/msf/pulls/1" {
		t.Errorf("path = %s", path)
	}
	for _, want := range []string{"the current description", "Scaffold a portal (sa-7dc)"} {
		if !strings.Contains(got, want) {
			t.Errorf("the request does not carry %q: %s", want, got)
		}
	}

	// Refused before a request, the way every other call in this file is, so
	// the caller gets a sentence rather than a 401 to interpret.
	if err := c.UpdatePullRequest(context.Background(), nil, "o", "r", 1, "t", "b"); err == nil {
		t.Error("a nil token must be refused")
	}
	if err := c.UpdatePullRequest(context.Background(), tok, "o", "r", 0, "t", "b"); err == nil {
		t.Error("a zero pull request number must be refused")
	}

	// A non-2xx is an error rather than a silent success: the whole point is
	// that the description matches, and a failure that reports ok leaves it
	// contradicting the diff with nobody told.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"message":"nope"}`)
	}))
	defer bad.Close()
	c2 := &Client{BaseURL: bad.URL, HTTPClient: bad.Client()}
	if err := c2.UpdatePullRequest(context.Background(), tok, "o", "r", 1, "t", "b"); err == nil {
		t.Error("a 422 must be reported, not swallowed")
	}
}
