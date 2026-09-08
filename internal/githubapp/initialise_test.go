package githubapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An empty repository is a perfectly good repository, and GitHub says so with a
// 409 from the commits endpoint. Reading that as "missing" or as a transport
// error is how a solvable state becomes a support question -- which is where
// the msf project stopped: project created, repository attached, and nothing
// could start because no commit existed to check out.
func TestInspectRepositoryTellsEmptyFromMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/empty":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": ""})
		case strings.HasPrefix(r.URL.Path, "/repos/acme/empty/commits"):
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"Git Repository is empty."}`))
		case r.URL.Path == "/repos/acme/full":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "trunk"})
		case strings.HasPrefix(r.URL.Path, "/repos/acme/full/commits"):
			_, _ = w.Write([]byte(`[{"sha":"abc"}]`))
		case r.URL.Path == "/repos/acme/gone":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	tok := &InstallationToken{Token: "t"}
	ctx := context.Background()

	st, err := c.InspectRepository(ctx, tok, "acme", "empty")
	if err != nil {
		t.Fatalf("an empty repository is not an error: %v", err)
	}
	if !st.Empty {
		t.Error("an empty repository did not report as empty")
	}
	// With no commits there is no branch, so the caller needs a name to create
	// one on. Failing here would leave the repository uninitialisable.
	if st.DefaultBranch != "main" {
		t.Errorf("default branch for an empty repository is %q, wanted main", st.DefaultBranch)
	}

	st, err = c.InspectRepository(ctx, tok, "acme", "full")
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	if st.Empty {
		t.Error("a repository with commits reported as empty")
	}
	// Not assumed to be main: initialising on the wrong branch would create a
	// second one and leave the default still empty.
	if st.DefaultBranch != "trunk" {
		t.Errorf("default branch is %q, wanted trunk", st.DefaultBranch)
	}

	if _, err := c.InspectRepository(ctx, tok, "acme", "gone"); err == nil {
		t.Error("a missing repository was not an error")
	} else if errors.Is(err, ErrRepositoryEmpty) {
		t.Error("a missing repository was reported as empty, which would make us " +
			"create a commit in a repository that does not exist")
	}
}

// The commit has to land on the branch the repository will actually use, with
// content, or the rig still cannot be created.
func TestInitialiseRepositoryCommitsToTheDefaultBranch(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Method != http.MethodPut {
			t.Errorf("method is %s, wanted PUT", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	if err := c.InitialiseRepository(context.Background(), &InstallationToken{Token: "t"},
		"acme", "new", "trunk", "Acme Data Portal"); err != nil {
		t.Fatalf("initialise: %v", err)
	}

	if path != "/repos/acme/new/contents/README.md" {
		t.Errorf("wrote to %q", path)
	}
	if got["branch"] != "trunk" {
		t.Errorf("committed to branch %v, not the repository's default", got["branch"])
	}
	raw, err := base64.StdEncoding.DecodeString(got["content"].(string))
	if err != nil {
		t.Fatalf("content is not base64: %v", err)
	}
	// Names the project and says why it exists. Somebody will find this commit
	// and need to know whether it was theirs.
	for _, want := range []string{"Acme Data Portal", "attached to a project", "Replace this file"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the README does not mention %q:\n%s", want, raw)
		}
	}
	if msg, _ := got["message"].(string); !strings.Contains(msg, "Initial commit") {
		t.Errorf("commit message is %q", msg)
	}
}

// A repository with no name is a caller bug, and creating a commit somewhere
// unintended is worse than refusing.
func TestInitialiseRepositoryRefusesIncompleteArguments(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1"}
	tok := &InstallationToken{Token: "t"}
	for _, tc := range []struct{ owner, repo string }{{"", "r"}, {"o", ""}} {
		if err := c.InitialiseRepository(context.Background(), tok, tc.owner, tc.repo, "main", "x"); err == nil {
			t.Errorf("owner=%q repo=%q was accepted", tc.owner, tc.repo)
		}
	}
	if err := c.InitialiseRepository(context.Background(), nil, "o", "r", "main", "x"); err == nil {
		t.Error("a nil token was accepted")
	}
}
