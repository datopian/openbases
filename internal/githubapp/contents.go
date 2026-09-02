package githubapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// The Contents and Git Refs endpoints, used to propose a file without cloning.
//
// The alternative is CloneURL plus git: a working directory on the node, a
// token in a remote, and a push. This is three API calls and no credential on
// disk, which for one small file is the cheaper and safer shape. It is not a
// general replacement -- a change spanning many files or needing a real merge
// wants a clone.

// BranchHead returns the commit a branch points at.
func (c *Client) BranchHead(ctx context.Context, tok *InstallationToken, owner, repo, branch string) (string, error) {
	if tok == nil || tok.Token == "" {
		return "", errors.New("no installation token")
	}
	if owner == "" || repo == "" || branch == "" {
		return "", errors.New("a branch lookup needs an owner, a repository and a branch")
	}

	u := fmt.Sprintf("%s/repos/%s/%s/git/ref/heads/%s",
		c.baseURL(), owner, repo, url.PathEscape(branch))
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.callJSON(ctx, tok, http.MethodGet, u, nil, &out, http.StatusOK); err != nil {
		return "", fmt.Errorf("reading branch %s: %w", branch, err)
	}
	if out.Object.SHA == "" {
		return "", fmt.Errorf("branch %s has no commit", branch)
	}
	return out.Object.SHA, nil
}

// CreateBranch points a new branch at a commit.
//
// An existing branch is not an error: the publisher retries, and a branch left
// behind by a pass that failed before opening the pull request is the state it
// has to be able to continue from.
func (c *Client) CreateBranch(ctx context.Context, tok *InstallationToken, owner, repo, branch, sha string) error {
	if tok == nil || tok.Token == "" {
		return errors.New("no installation token")
	}
	if branch == "" || sha == "" {
		return errors.New("a branch needs a name and a commit")
	}

	body, err := json.Marshal(map[string]string{"ref": "refs/heads/" + branch, "sha": sha})
	if err != nil {
		return err
	}
	u := fmt.Sprintf("%s/repos/%s/%s/git/refs", c.baseURL(), owner, repo)
	err = c.callJSON(ctx, tok, http.MethodPost, u, body, nil, http.StatusCreated)
	if err != nil && strings.Contains(err.Error(), "Reference already exists") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating branch %s: %w", branch, err)
	}
	return nil
}

// FileSHA returns the blob SHA of a file on a branch, or "" when it does not
// exist there.
func (c *Client) FileSHA(ctx context.Context, tok *InstallationToken, owner, repo, branch, path string) (string, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		c.baseURL(), owner, repo, escapePath(path), url.QueryEscape(branch))
	var out struct {
		SHA string `json:"sha"`
	}
	err := c.callJSON(ctx, tok, http.MethodGet, u, nil, &out, http.StatusOK)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return "", nil
		}
		return "", err
	}
	return out.SHA, nil
}

// PutFile writes a file on a branch, creating or replacing it.
//
// Replacing requires the blob SHA of what is there, which is GitHub's
// optimistic lock: without it a write refuses rather than silently discarding
// somebody's edit. So an existing file is looked up first.
func (c *Client) PutFile(ctx context.Context, tok *InstallationToken, owner, repo, branch, path, message string, content []byte) error {
	switch {
	case tok == nil || tok.Token == "":
		return errors.New("no installation token")
	case owner == "" || repo == "" || branch == "" || path == "":
		return errors.New("writing a file needs an owner, a repository, a branch and a path")
	case strings.TrimSpace(message) == "":
		return errors.New("a commit needs a message")
	case len(content) == 0:
		return errors.New("refusing to commit an empty file")
	}

	existing, err := c.FileSHA(ctx, tok, owner, repo, branch, path)
	if err != nil {
		return fmt.Errorf("checking for an existing %s: %w", path, err)
	}

	payload := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  branch,
	}
	if existing != "" {
		payload["sha"] = existing
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s", c.baseURL(), owner, repo, escapePath(path))
	// 201 for a new file, 200 for a replacement.
	if err := c.callJSON(ctx, tok, http.MethodPut, u, body, nil,
		http.StatusCreated, http.StatusOK); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// escapePath escapes each segment, so a path keeps its slashes.
func escapePath(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// callJSON is the shared request shape: App token, pinned API version, and an
// error that carries GitHub's own explanation without carrying the token.
func (c *Client) callJSON(ctx context.Context, tok *InstallationToken, method, u string,
	body []byte, out any, okCodes ...int) error {

	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
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
	defer resp.Body.Close()

	for _, code := range okCodes {
		if resp.StatusCode == code {
			if out == nil {
				return nil
			}
			return json.NewDecoder(resp.Body).Decode(out)
		}
	}

	var detail struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	json.NewDecoder(resp.Body).Decode(&detail)
	reasons := detail.Message
	for _, e := range detail.Errors {
		if e.Message != "" {
			reasons += "; " + e.Message
		}
	}
	return fmt.Errorf("%s %s: %s", resp.Status, method, reasons)
}
