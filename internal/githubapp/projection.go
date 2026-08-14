package githubapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrRepositoryNotRegistered reports an event for a repository the control
// plane does not know.
//
// Such an event is dropped rather than projected. A repository row carries the
// project it belongs to, and the project carries the visibility class that
// every read policy is built on — so inventing one would create a row no
// row-level security rule can classify, visible to whoever queries widest.
var ErrRepositoryNotRegistered = errors.New("repository is not registered to a project")

// pullRequestEvent is the subset of GitHub's pull_request payload we project.
type pullRequestEvent struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number    int        `json:"number"`
		Title     string     `json:"title"`
		State     string     `json:"state"`
		Merged    bool       `json:"merged"`
		MergedAt  *time.Time `json:"merged_at"`
		UpdatedAt time.Time  `json:"updated_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository repository `json:"repository"`
}

// repository identifies the repository an event came from.
//
// GitHub's numeric ID is stable across renames and transfers; full_name is not.
// The schema keeps provider_id for exactly that reason, so both are carried.
type repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

// owner and name, split from full_name.
func (r repository) split() (string, string) {
	owner, name, ok := strings.Cut(r.FullName, "/")
	if !ok {
		return "", ""
	}
	return owner, name
}

// projectedState collapses GitHub's state and merged flag into ours.
//
// GitHub reports state as only "open" or "closed"; whether a pull request was
// merged lives in a separate boolean. Projecting state alone would record every
// merge as a plain close and quietly erase the distinction between work that
// landed and work that was abandoned.
func (e pullRequestEvent) projectedState() string {
	if e.PullRequest.Merged || e.PullRequest.MergedAt != nil {
		return "merged"
	}
	if e.PullRequest.State == "closed" {
		return "closed"
	}
	return "open"
}

// ProjectPullRequest records a pull_request event against its repository.
//
// Returns false when the event was ignored as stale or irrelevant, so the
// caller can distinguish "nothing to do" from "failed".
func ProjectPullRequest(ctx context.Context, db *sql.DB, payload []byte) (bool, error) {
	var e pullRequestEvent
	if err := json.Unmarshal(payload, &e); err != nil {
		return false, fmt.Errorf("decoding pull_request event: %w", err)
	}
	if e.PullRequest.Number == 0 || e.Repository.FullName == "" {
		return false, errors.New("pull_request event carries no number or repository")
	}

	// Projection runs through a SECURITY DEFINER function.
	//
	// Row-level security is built on current_app_user(), and a webhook has no
	// user: GitHub is not a member of a project. Querying these tables with no
	// identity simply returns nothing, which is how every event was recorded as
	// "repository not registered" while the repository plainly was registered.
	owner, name := e.Repository.split()
	if owner == "" {
		return false, fmt.Errorf("%w: malformed repository name %q",
			ErrRepositoryNotRegistered, e.Repository.FullName)
	}

	var applied sql.NullBool
	err := db.QueryRowContext(ctx,
		`SELECT project_pull_request($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		providerID(e.Repository.ID), owner, name,
		e.PullRequest.Number, e.PullRequest.Title, e.projectedState(),
		e.PullRequest.User.Login, e.PullRequest.Head.SHA,
		e.PullRequest.MergedAt, e.PullRequest.UpdatedAt).Scan(&applied)
	if err != nil {
		return false, err
	}
	// NULL means no such repository — distinct from false, which means the
	// event was older than what is already recorded.
	if !applied.Valid {
		return false, fmt.Errorf("%w: %s", ErrRepositoryNotRegistered, e.Repository.FullName)
	}
	return applied.Bool, nil
}

// checkSuiteEvent is the subset of check_suite and check_run payloads we use.
type checkSuiteEvent struct {
	CheckSuite struct {
		HeadSHA    string    `json:"head_sha"`
		Status     string    `json:"status"`
		Conclusion string    `json:"conclusion"`
		UpdatedAt  time.Time `json:"updated_at"`
	} `json:"check_suite"`
	Repository repository `json:"repository"`
}

// checksState maps GitHub's status and conclusion onto our four values.
//
// A suite that has not completed is pending whatever its conclusion field says,
// because GitHub leaves the previous conclusion in place while a re-run is in
// flight. Reading conclusion alone would report the OLD result as though it
// were the current one.
func (e checkSuiteEvent) checksState() string {
	if e.CheckSuite.Status != "completed" {
		return "pending"
	}
	switch e.CheckSuite.Conclusion {
	case "success":
		return "success"
	case "failure", "timed_out", "action_required", "startup_failure":
		return "failure"
	default:
		// cancelled, skipped, neutral, stale — not a pass, not a failure.
		return "neutral"
	}
}

// ProjectCheckSuite records CI state against the pull request at that commit.
//
// Matching on head_sha rather than on a pull request number is deliberate: a
// check suite belongs to a commit, and after a force-push the older suite is no
// longer about the code under review. Keying on the commit lets a stale suite
// simply match nothing.
func ProjectCheckSuite(ctx context.Context, db *sql.DB, payload []byte) (bool, error) {
	var e checkSuiteEvent
	if err := json.Unmarshal(payload, &e); err != nil {
		return false, fmt.Errorf("decoding check_suite event: %w", err)
	}
	if e.CheckSuite.HeadSHA == "" || e.Repository.FullName == "" {
		return false, errors.New("check_suite event carries no head SHA or repository")
	}

	owner, name := e.Repository.split()
	if owner == "" {
		return false, fmt.Errorf("%w: malformed repository name %q",
			ErrRepositoryNotRegistered, e.Repository.FullName)
	}

	var applied sql.NullBool
	err := db.QueryRowContext(ctx,
		`SELECT project_check_suite($1, $2, $3, $4, $5)`,
		providerID(e.Repository.ID), owner, name,
		e.CheckSuite.HeadSHA, e.checksState()).Scan(&applied)
	if err != nil {
		return false, err
	}
	if !applied.Valid {
		return false, fmt.Errorf("%w: %s", ErrRepositoryNotRegistered, e.Repository.FullName)
	}
	return applied.Bool, nil
}

// providerID renders GitHub's numeric repository ID for storage.
//
// It is kept as text because the column is text, and returns nil for a payload
// that carries no ID so the lookup falls back to owner and name rather than
// matching a literal "0".
func providerID(id int64) any {
	if id == 0 {
		return nil
	}
	return strconv.FormatInt(id, 10)
}
