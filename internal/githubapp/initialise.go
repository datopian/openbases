package githubapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrRepositoryEmpty is returned when a repository has no commits.
//
// Distinguished from "does not exist" and from "not accessible" because the
// three need different answers: create a commit, fix the name, or fix the
// installation.
var ErrRepositoryEmpty = errors.New("repository has no commits")

// RepositoryState is what we need to know before a rig can be made from a
// repository.
type RepositoryState struct {
	DefaultBranch string
	// Empty is true when the repository exists and has no commits.
	Empty bool
}

// InspectRepository reports whether a repository can be cloned into a rig.
//
// GitHub answers this awkwardly: an empty repository is a perfectly good
// repository that returns 409 from the commits endpoint, with a body saying
// "Git Repository is empty". So the state is read from the repository itself and
// the emptiness from the commit list, because `size` is 0 for a while after the
// first push and cannot be trusted for this.
func (c *Client) InspectRepository(ctx context.Context, tok *InstallationToken, owner, repo string) (RepositoryState, error) {
	var out RepositoryState
	if tok == nil || tok.Token == "" {
		return out, errors.New("no installation token")
	}
	if owner == "" || repo == "" {
		return out, errors.New("inspecting a repository needs an owner and a name")
	}

	var meta struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.githubJSON(ctx, tok, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s", owner, repo), nil, &meta); err != nil {
		return out, err
	}
	out.DefaultBranch = meta.DefaultBranch
	if out.DefaultBranch == "" {
		// A repository with no commits has no branches, and GitHub reports the
		// name it WILL use. Falling back rather than failing keeps the caller's
		// next step -- create a commit on that branch -- possible.
		out.DefaultBranch = "main"
	}

	err := c.githubJSON(ctx, tok, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/commits?per_page=1", owner, repo), nil, nil)
	switch {
	case err == nil:
		return out, nil
	case errors.Is(err, ErrRepositoryEmpty):
		out.Empty = true
		return out, nil
	default:
		return out, err
	}
}

// InitialiseRepository puts a first commit in an empty repository.
//
// A repository with no commits cannot become a rig: `gt rig add` refuses it
// with "repository is empty (no commits). Push at least one commit before
// adding it as a rig", and there is nothing for an agent to check out. That is
// where the msf project stopped -- the project existed, the repository was
// attached, and the work could not start.
//
// Creating a README is how GitHub itself initialises a repository, and it is
// the smallest thing that makes the default branch exist. The alternative --
// telling whoever attached the repository to go and push something -- turns a
// solvable state into a support question, and they attached it precisely because
// they wanted work to happen there.
//
// Idempotent by virtue of being conditional: the caller inspects first, and
// GitHub refuses a create on a path that already exists.
func (c *Client) InitialiseRepository(ctx context.Context, tok *InstallationToken, owner, repo, branch, projectName string) error {
	if tok == nil || tok.Token == "" {
		return errors.New("no installation token")
	}
	if owner == "" || repo == "" {
		return errors.New("initialising a repository needs an owner and a name")
	}
	if branch == "" {
		branch = "main"
	}

	name := projectName
	if strings.TrimSpace(name) == "" {
		name = repo
	}

	// Says what it is and why it appeared, because somebody will find this
	// commit and wonder. No agent instructions and no configuration: the file
	// exists to give the branch a commit, and anything else in it would be
	// content nobody asked for.
	readme := fmt.Sprintf(`# %s

Created by OpenBases when this repository was attached to a project, because a
repository with no commits cannot be checked out and no work could start in it.

Replace this file with whatever the project actually needs.
`, name)

	body, err := json.Marshal(map[string]any{
		"message": "Initial commit\n\nCreated so the repository can be checked out for work.",
		"content": base64.StdEncoding.EncodeToString([]byte(readme)),
		"branch":  branch,
	})
	if err != nil {
		return err
	}

	return c.githubJSON(ctx, tok, http.MethodPut,
		fmt.Sprintf("/repos/%s/%s/contents/README.md", owner, repo), body, nil)
}

// githubJSON performs one authenticated API call.
//
// out may be nil when only the status matters. The 409-with-"empty" case is
// translated here rather than at each call site: it is the one status whose
// meaning is not obvious from the code, and every caller needs the same answer.
func (c *Client) githubJSON(ctx context.Context, tok *InstallationToken, method, path string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}

	// Read the message: GitHub's is more specific than any status mapping, and
	// it is what tells an empty repository apart from a missing one.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode == http.StatusConflict &&
		strings.Contains(strings.ToLower(string(snippet)), "empty") {
		return ErrRepositoryEmpty
	}
	return fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status,
		strings.TrimSpace(string(snippet)))
}
