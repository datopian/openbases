package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/datopian/openbases/internal/domain"
)

// rawIssue mirrors what `bd --json` emits.
//
// Beads is used without forking, and its JSON is the contract (ADR-0003). The
// fields are decoded loosely: an unknown field is ignored rather than fatal, so
// an upstream addition does not break the adapter, while a REMOVED field shows
// up as a zero value and is caught by the contract tests run against the pinned
// binary.
type rawIssue struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	IssueType   string   `json:"issue_type"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	Labels      []string `json:"labels"`
	Owner       string   `json:"owner"`
	ExternalRef string   `json:"external_ref"`
	Parent      string   `json:"parent"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

func (r rawIssue) toIssue(db DatabaseRef, org string) Issue {
	created, _ := time.Parse(time.RFC3339, r.CreatedAt)
	updated, _ := time.Parse(time.RFC3339, r.UpdatedAt)
	return Issue{
		Ref: domain.WorkRef{
			OrganisationID:  org,
			ExecutionCellID: db.CellID,
			BeadsDatabaseID: db.ID,
			BeadID:          r.ID,
		},
		Title:       r.Title,
		Description: r.Description,
		Type:        r.IssueType,
		Status:      r.Status,
		Priority:    r.Priority,
		Labels:      r.Labels,
		Assignee:    r.Owner,
		ExternalRef: r.ExternalRef,
		CreatedAt:   created,
		UpdatedAt:   updated,
	}
}

// run executes a `bd` command and records it.
//
// Every invocation is captured with command, actor, database, duration and
// result (plan section 7.1), because a failed write has to be reconcilable and
// an unrecorded one cannot be.
func (c *CLIClient) run(ctx context.Context, db DatabaseRef, args ...string) ([]byte, error) {
	if c.Binary == "" {
		return nil, errors.New("no bd binary configured")
	}

	// -C scopes the command to the database's working directory. Beads
	// discovers its database from the directory, so running without this would
	// operate on whichever database happened to be nearby.
	full := append([]string{"-C", db.Path}, args...)

	cmd := exec.CommandContext(ctx, c.Binary, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	// Beads writes into a Dolt database owned by the cell user. The actor is
	// passed through so the Dolt commit trail attributes the change to whoever
	// asked for it rather than to the service account.
	env := cmd.Environ()
	if c.Actor != "" {
		env = append(env, "BEADS_ACTOR="+c.Actor)
	}
	if c.Home != "" {
		// An explicit home, for a caller that knows where the cell's is.
		//
		// The node's is the CELL's home, not the database directory: Dolt
		// reads its configuration from HOME and segfaults without one, and
		// the dispatcher has always passed HOME=<cellRoot> when it shells out
		// to bd. A filing path that used the database directory instead would
		// be a second, subtly different environment for the same binary.
		env = append(env, "HOME="+c.Home)
	}
	if c.HomeAtDatabasePath && db.Path != "" {
		// Last wins in exec's environment, so this overrides an inherited
		// HOME rather than conflicting with it.
		env = append(env, "HOME="+db.Path)
	}
	cmd.Env = env

	start := time.Now()
	err := cmd.Run()
	record := CommandRecord{
		Command:  full,
		Actor:    c.Actor,
		Cell:     db.CellID,
		Database: db.ID,
		Duration: time.Since(start),
		ExitCode: cmd.ProcessState.ExitCode(),
		Err:      err,
	}
	if c.Record != nil {
		c.Record(record)
	}

	if err != nil {
		// stderr is included because bd's errors are actionable and contain no
		// credentials; the command itself carries no secret either.
		return nil, fmt.Errorf("bd %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (c *CLIClient) decodeIssues(out []byte, db DatabaseRef) ([]Issue, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var raw []rawIssue
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return nil, fmt.Errorf("decoding bd output: %w", err)
	}
	issues := make([]Issue, 0, len(raw))
	for _, r := range raw {
		issues = append(issues, r.toIssue(db, c.OrganisationID))
	}
	return issues, nil
}
