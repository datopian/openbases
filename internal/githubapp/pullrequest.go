package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
		return nil, fmt.Errorf("opening pull request: %s: %s", resp.Status, reasons)
	}

	var pr PullRequest
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return &pr, nil
}
