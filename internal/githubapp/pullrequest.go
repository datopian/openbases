package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// CloneURL returns a URL an agent can push with, carrying a short-lived token.
//
// The token is embedded because git has no other reliable way to authenticate a
// non-interactive push, and it is an INSTALLATION token: it expires within the
// hour and is scoped to the repositories the App is installed on. A personal
// access token in the same position would be long-lived and far broader.
//
// The returned string contains a live credential, so it must never be logged,
// echoed into a command that gets recorded, or written into a git remote that
// persists — git stores remotes in plain text in .git/config.
func CloneURL(tok *InstallationToken, owner, repo string) (string, error) {
	if tok == nil || tok.Token == "" {
		return "", errors.New("no installation token")
	}
	// x-access-token is the documented username for an App token.
	return fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git", tok.Token, owner, repo), nil
}

// PullRequest is the subset of a created pull request we keep.
type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

// OpenPullRequestRequest describes the pull request to open.
type OpenPullRequestRequest struct {
	Owner string
	Repo  string
	Head  string // the branch the agent pushed
	Base  string
	Title string
	Body  string
}

// OpenPullRequest opens a pull request as the App.
//
// Attribution matters here: a pull request opened with an installation token is
// authored by the App, not by a person, so it is visibly agent work in the
// GitHub UI without anyone having to annotate it (plan section 9.2).
func (c *Client) OpenPullRequest(ctx context.Context, tok *InstallationToken, req OpenPullRequestRequest) (*PullRequest, error) {
	switch {
	case tok == nil || tok.Token == "":
		return nil, errors.New("no installation token")
	case req.Owner == "" || req.Repo == "":
		return nil, errors.New("a pull request needs an owner and a repository")
	case req.Head == "" || req.Base == "":
		return nil, errors.New("a pull request needs a head and a base branch")
	case strings.TrimSpace(req.Title) == "":
		return nil, errors.New("a pull request needs a title")
	case req.Head == req.Base:
		// GitHub rejects this too, but catching it here names the mistake
		// rather than surfacing a 422 the agent has to interpret.
		return nil, fmt.Errorf("head and base are both %q; the agent did not create a branch", req.Head)
	}

	body, err := json.Marshal(map[string]any{
		"title": req.Title,
		"head":  req.Head,
		"base":  req.Base,
		"body":  req.Body,
		// Never open as a draft by default. A draft does not run required
		// checks on some configurations, and the point of the flow is that
		// agent work is reviewed exactly like anyone else's.
		"draft": false,
	})
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/repos/%s/%s/pulls", c.baseURL(), req.Owner, req.Repo)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+tok.Token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		// GitHub's 422 for this endpoint names the actual problem — no commits
		// between branches, a branch that does not exist — so it is worth
		// surfacing. It echoes the request's field names, not the token.
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
		// GitHub says "A pull request already exists for owner:branch". That
		// is the retry path rather than a failure, and the caller has to be
		// able to tell them apart to go and find the one it opened before.
		if strings.Contains(reasons, "already exists") {
			return nil, fmt.Errorf("%w: %s", ErrAlreadyOpen, reasons)
		}
		return nil, fmt.Errorf("opening pull request: %s: %s", resp.Status, reasons)
	}

	var pr PullRequest
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// ErrAlreadyOpen reports a pull request that exists for this head branch.
//
// Its own error because it is the retry path, not a failure: a pass that opened
// the pull request and then died before recording it comes back to find its own
// work.
var ErrAlreadyOpen = errors.New("a pull request is already open for this branch")

// FindPullRequest returns the open pull request for a head branch, if there is
// one.
func (c *Client) FindPullRequest(ctx context.Context, tok *InstallationToken, owner, repo, head string) (*PullRequest, error) {
	if tok == nil || tok.Token == "" {
		return nil, errors.New("no installation token")
	}
	if owner == "" || repo == "" || head == "" {
		return nil, errors.New("finding a pull request needs an owner, a repository and a head branch")
	}

	u := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&head=%s",
		c.baseURL(), owner, repo, url.QueryEscape(owner+":"+head))
	var out []PullRequest
	if err := c.callJSON(ctx, tok, http.MethodGet, u, nil, &out, http.StatusOK); err != nil {
		return nil, fmt.Errorf("looking for a pull request from %s: %w", head, err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// UpdatePullRequest rewrites an open pull request's title and body.
//
// A re-dispatched bead pushes to the same branch and reuses its pull request,
// which is deliberate -- work re-dispatched must not multiply pull requests.
// But the description was written by the FIRST landing and never touched
// again, so it went stale the moment a second run changed the diff:
// datopian/msf#1 said `Files: README.md, ARCHITECTURE.md, docs/,
// node_modules/, portal/` for hours after the run that produced that list had
// been superseded, and the node_modules it named was no longer in the branch.
//
// A description that contradicts the diff beside it is worse than none: a
// reviewer cannot tell which of the two is out of date.
func (c *Client) UpdatePullRequest(ctx context.Context, tok *InstallationToken,
	owner, repo string, number int, title, body string) error {

	if tok == nil || tok.Token == "" {
		return errors.New("no installation token")
	}
	if owner == "" || repo == "" || number == 0 {
		return errors.New("updating a pull request needs an owner, a repository and a number")
	}

	payload, err := json.Marshal(map[string]any{"title": title, "body": body})
	if err != nil {
		return err
	}
	u := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL(), owner, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		reasons, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("updating pull request %d: %s: %s", number, resp.Status, strings.TrimSpace(string(reasons)))
	}
	return nil
}
