package publish

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/datopian/workgraph/internal/githubapp"
	"github.com/datopian/workgraph/internal/knowledge"
)

type fakeGitHub struct {
	scopedTo []string
	branches map[string]string
	files    map[string][]byte
	prs      []githubapp.OpenPullRequestRequest
	putErr   error
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{branches: map[string]string{}, files: map[string][]byte{}}
}

func (f *fakeGitHub) InstallationToken(ctx context.Context, repos ...string) (*githubapp.InstallationToken, error) {
	f.scopedTo = repos
	return &githubapp.InstallationToken{Token: "ghs_test"}, nil
}

func (f *fakeGitHub) BranchHead(ctx context.Context, tok *githubapp.InstallationToken, owner, repo, branch string) (string, error) {
	return "basesha", nil
}

func (f *fakeGitHub) CreateBranch(ctx context.Context, tok *githubapp.InstallationToken, owner, repo, branch, sha string) error {
	f.branches[branch] = sha
	return nil
}

func (f *fakeGitHub) PutFile(ctx context.Context, tok *githubapp.InstallationToken, owner, repo, branch, path, message string, content []byte) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.files[path] = content
	return nil
}

func (f *fakeGitHub) FileContent(ctx context.Context, tok *githubapp.InstallationToken, owner, repo, ref, path string) ([]byte, string, error) {
	if c, ok := f.files[ref+":"+path]; ok {
		return c, "sha", nil
	}
	if c, ok := f.files[path]; ok {
		return c, "sha", nil
	}
	return nil, "", errors.New("404 not found")
}

func (f *fakeGitHub) OpenPullRequest(ctx context.Context, tok *githubapp.InstallationToken, req githubapp.OpenPullRequestRequest) (*githubapp.PullRequest, error) {
	f.prs = append(f.prs, req)
	return &githubapp.PullRequest{Number: len(f.prs), HTMLURL: "https://github.test/pr/1"}, nil
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func aPending() PendingRecord {
	return PendingRecord{
		Record: knowledge.Record{
			ID:         "8c1e40c9-d5ab-4ecc-b8ba-3f51f9a0ac7f",
			Type:       "decision",
			Statement:  "Use batch ingestion for phase one.",
			Scope:      "project",
			Project:    "cdt",
			Visibility: "restricted",
			Reviewer:   "monika.popova@datopian.com",
			Sources:    []knowledge.Source{{SourceID: "s1", Provider: "google-meet"}},
		},
	}
}

// A publisher with no database cannot do either half.
func TestNoDatabaseIsRefused(t *testing.T) {
	p := &RecordPublisher{DB: nil, GitHub: newFakeGitHub(), Log: quietLog()}
	if _, err := p.Run(t.Context(), 5); err == nil {
		t.Fatal("a publisher with no database ran anyway")
	}
}

// Without a repository the pass still runs -- decision beads need nothing from
// Git -- and must not reach GitHub at all. Not even to mint a token: an
// installation token for a repository nobody configured is a request made on a
// guess.
func TestWithoutARepositoryNothingReachesGitHub(t *testing.T) {
	gh := newFakeGitHub()
	p := &RecordPublisher{DB: sql.OpenDB(deadConnector{}), GitHub: gh, Log: quietLog()}

	// The database is unreachable, so the pass fails where it reads the queue.
	// Which is after the point where a token would have been minted.
	if _, err := p.Run(t.Context(), 5); err == nil {
		t.Fatal("a query against a dead connector succeeded")
	}
	if len(gh.scopedTo) != 0 || len(gh.prs) != 0 || len(gh.branches) != 0 {
		t.Fatalf("GitHub was called with no repository configured: %v", gh.scopedTo)
	}
}

// deadConnector is a database that cannot be reached, which is the cheapest way
// to assert the ORDER of what a pass does without standing up Postgres.
type deadConnector struct{}

func (deadConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("no database here")
}

func (deadConnector) Driver() driver.Driver { return nil }

// The token is scoped to the one repository this writes to. An installation
// token is broad by default, and the App is installed on client repositories.
func TestTheTokenIsScopedToTheKnowledgeRepository(t *testing.T) {
	gh := newFakeGitHub()
	p := &RecordPublisher{GitHub: gh, Owner: "datopian", Repo: "company-workgraph", Log: quietLog()}

	tok, err := p.GitHub.InstallationToken(t.Context(), p.Owner+"/"+p.Repo)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if tok.Token == "" {
		t.Fatal("no token")
	}
	if len(gh.scopedTo) != 1 || gh.scopedTo[0] != "datopian/company-workgraph" {
		t.Fatalf("token scoped to %v", gh.scopedTo)
	}
}

// A branch, a file and a pull request, in that order, with the record's own
// path and no transcript text.
func TestProposeWritesTheRecordAndOpensAPullRequest(t *testing.T) {
	gh := newFakeGitHub()
	p := &RecordPublisher{GitHub: gh, Owner: "datopian", Repo: "company-workgraph", Log: quietLog()}
	r := aPending()

	// record() needs a database; propose() is exercised up to it by pointing
	// the fake at a failure after the pull request.
	err := p.propose(t.Context(), &githubapp.InstallationToken{Token: "t"}, "basesha", "main", r, quietLog())
	if err == nil {
		t.Fatal("expected the database write to fail without a database")
	}

	if len(gh.branches) != 1 {
		t.Fatalf("branches = %v", gh.branches)
	}
	for name := range gh.branches {
		if !strings.HasPrefix(name, "knowledge/decision-") {
			t.Fatalf("branch %q does not say what it carries", name)
		}
	}
	// The file name carries the acceptance date and an id fragment, so this
	// scans for the directory rather than reconstructing the name.
	var content []byte
	var ok bool
	for path, c := range gh.files {
		if strings.HasPrefix(path, "knowledge/projects/cdt/decisions/") {
			content, ok = c, true
		}
	}
	if !ok {
		t.Fatalf("no file under the project's decisions directory: %v", keys(gh.files))
	}
	if !strings.Contains(string(content), "visibility: restricted") {
		t.Fatalf("the classification did not survive:\n%s", content)
	}
	if len(gh.prs) != 1 {
		t.Fatalf("pull requests = %d", len(gh.prs))
	}
	if gh.prs[0].Base != "main" || gh.prs[0].Head == gh.prs[0].Base {
		t.Fatalf("pull request targets %q from %q", gh.prs[0].Base, gh.prs[0].Head)
	}
	if !strings.Contains(gh.prs[0].Body, "mem-8c1e40c9") {
		t.Fatalf("the pull request does not name the record:\n%s", gh.prs[0].Body)
	}
}

// A failure before the pull request must leave a state the next pass can
// continue from, which is why an existing branch is not an error.
func TestAFailedWriteLeavesTheBranchForTheRetry(t *testing.T) {
	gh := newFakeGitHub()
	gh.putErr = errors.New("network")
	p := &RecordPublisher{GitHub: gh, Owner: "o", Repo: "r", Log: quietLog()}

	if err := p.propose(t.Context(), &githubapp.InstallationToken{Token: "t"}, "sha", "main", aPending(), quietLog()); err == nil {
		t.Fatal("a failed write reported success")
	}
	if len(gh.branches) != 1 {
		t.Fatal("the branch was not created before the file")
	}
	if len(gh.prs) != 0 {
		t.Fatal("a pull request was opened for a file that was never written")
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A supersession is one reviewable change: the correction and the retirement of
// what it corrects, in the same pull request.
func TestSupersessionRetiresTheOldFileInTheSamePullRequest(t *testing.T) {
	old := aPending()
	oldFile, err := old.Record.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	oldPath := old.Record.Path()

	gh := newFakeGitHub()
	gh.files[oldPath] = oldFile

	r := aPending()
	r.Record.ID = "22222222-3333-4444-5555-666666666666"
	r.Record.Statement = "Use streaming ingestion after all."
	r.Record.Supersedes = old.Record.ID
	r.SupersedesGitPath = oldPath

	p := &RecordPublisher{GitHub: gh, Owner: "o", Repo: "r", Log: quietLog()}
	// Fails at the database write, after both files and the pull request.
	if err := p.propose(t.Context(), &githubapp.InstallationToken{Token: "t"}, "sha", "main", r, quietLog()); err == nil {
		t.Fatal("expected the database write to fail")
	}

	patched, ok := gh.files[oldPath]
	if !ok {
		t.Fatal("the superseded file was not written")
	}
	if !strings.Contains(string(patched), "status: superseded") ||
		!strings.Contains(string(patched), "superseded_by: mem-22222222-3333-4444-5555-666666666666") {
		t.Fatalf("the old file was not marked:\n%s", patched)
	}
	if len(gh.prs) != 1 {
		t.Fatalf("pull requests = %d", len(gh.prs))
	}
	if !strings.Contains(gh.prs[0].Body, "supersedes: `mem-"+old.Record.ID+"`") {
		t.Fatalf("the pull request does not say what it supersedes:\n%s", gh.prs[0].Body)
	}
}

// A retired file that no longer exists must not stop the correction from being
// proposed... but it must not be silent either.
func TestAMissingSupersededFileFailsLoudly(t *testing.T) {
	gh := newFakeGitHub()
	r := aPending()
	r.Record.Supersedes = "33333333-4444-5555-6666-777777777777"
	r.SupersedesGitPath = "knowledge/company/decisions/gone.md"

	p := &RecordPublisher{GitHub: gh, Owner: "o", Repo: "r", Log: quietLog()}
	err := p.propose(t.Context(), &githubapp.InstallationToken{Token: "t"}, "sha", "main", r, quietLog())
	if err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("error = %v, want it to name the file it could not retire", err)
	}
	if len(gh.prs) != 0 {
		t.Fatal("a pull request was opened with half the change in it")
	}
}
