// Package beads adapts the `bd` CLI.
//
// Beads is used without forking. Writes go through `bd` commands only; the
// control plane never mutates Dolt tables directly. Reads prefer `bd ... --json`
// for contract stability, and any read-only SQL stays behind this adapter with
// version-specific tests (plan section 7.1).
package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/datopian/workgraph/internal/domain"
)

// ErrNotImplemented marks an operation that WP-D2 has not yet delivered.
var ErrNotImplemented = errors.New("beads adapter not implemented (WP-D2)")

// DatabaseRef identifies one Beads database in the registry.
type DatabaseRef struct {
	ID     string
	Name   string
	CellID string
	Scope  domain.Scope
	// Path is the working directory the database lives under. Beads discovers
	// its database from the directory, so every invocation is scoped with -C
	// rather than relying on the process working directory.
	Path     string
	DoltPort int
}

// Issue is a work item as the control plane sees it.
type Issue struct {
	Ref         domain.WorkRef
	Title       string
	Description string
	Type        string
	Status      string
	Priority    int
	Labels      []string
	Assignee    string
	// Due is the date the work is expected by, if the source named one. Zero
	// means no due date rather than "today".
	Due time.Time
	// ExternalRef points back at whatever outside Beads caused this item, in
	// bd's own external reference field. A bead that came from an accepted
	// meeting statement should say so from inside the graph, without a lookup
	// in the control plane.
	ExternalRef string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CommandRecord captures one `bd` invocation for audit and reconciliation.
// Every write records command, actor, cell, database, version, duration and
// result (plan section 7.1).
type CommandRecord struct {
	Command  []string
	Actor    string
	Cell     string
	Database string
	Version  string
	Duration time.Duration
	ExitCode int
	Err      error
}

// Client is the contract for reading and writing work state.
type Client interface {
	// Ready returns unblocked work. `bd ready` remains authoritative for
	// readiness inside a graph; the control plane does not recompute it.
	Ready(ctx context.Context, db DatabaseRef) ([]Issue, error)
	Get(ctx context.Context, ref domain.WorkRef) (Issue, error)
	Create(ctx context.Context, db DatabaseRef, issue Issue) (domain.WorkRef, error)
	Update(ctx context.Context, ref domain.WorkRef, issue Issue) error
	Close(ctx context.Context, ref domain.WorkRef, evidence string) error
	// Link records a dependency inside one database. Relationships that cross
	// databases are stored in the control plane `work_links` table instead,
	// because Beads databases are isolated (plan section 2.3).
	Link(ctx context.Context, from, to domain.WorkRef, relation string) error
	// Backup runs the Beads native backup. The JSONL export is an additional
	// recovery aid, never the primary backup (plan section 15.3).
	Backup(ctx context.Context, db DatabaseRef) error

	// List returns issues, optionally filtered by status.
	List(ctx context.Context, db DatabaseRef, status string) ([]Issue, error)
}

// CLIClient drives the pinned `bd` binary.
type CLIClient struct {
	// Binary is the absolute path to the pinned `bd` executable, verified
	// against versions.lock before use.
	Binary string

	// OrganisationID is stamped into every work reference. A bead ID alone is
	// not a stable identity: project prefixes collide across databases
	// (plan section 7.3).
	OrganisationID string

	// Actor attributes writes in the Dolt commit trail. Without it every change
	// appears to come from the service account, which makes the history useless
	// for answering who did what.
	Actor string

	// DatabasePaths maps a Beads database ID to its working directory, loaded
	// from the registry.
	DatabasePaths map[string]string

	// HomeAtDatabasePath runs `bd` with HOME set to the database's own
	// directory.
	//
	// Dolt writes its configuration under $HOME and segfaults if it cannot.
	// The control node's service account is created without a home directory
	// -- correct for a service, fatal here -- so beads_hq initialises each
	// graph with HOME pointing at the graph directory, and its dolt config
	// lives at <path>/.dolt/config_global.json. Anything running `bd` there
	// has to use the same HOME or Dolt looks for a config it cannot write.
	//
	// Off by default: on a cell the user has a real home and a fresh config
	// under the graph directory would be a second identity to keep in step.
	HomeAtDatabasePath bool

	// Record receives one entry per invocation for the audit log.
	Record func(CommandRecord)
}

var _ Client = (*CLIClient)(nil)

// WithActor returns a client that attributes its writes to somebody else.
//
// A copy rather than a setter: the actor belongs to one operation, and a
// long-lived client whose actor is mutated between calls attributes whatever
// is in flight to whoever set it last.
func (c *CLIClient) WithActor(actor string) *CLIClient {
	copied := *c
	copied.Actor = actor
	return &copied
}

// Ready returns unblocked work.
//
// `bd ready` is authoritative for readiness inside a graph and the control
// plane does not recompute it (ADR-0003). Reimplementing the dependency walk
// here would mean two answers to "what can be worked on", and they would
// eventually disagree.
func (c *CLIClient) Ready(ctx context.Context, db DatabaseRef) ([]Issue, error) {
	out, err := c.run(ctx, db, "ready", "--json")
	if err != nil {
		return nil, err
	}
	return c.decodeIssues(out, db)
}

// List returns issues, optionally filtered by status.
func (c *CLIClient) List(ctx context.Context, db DatabaseRef, status string) ([]Issue, error) {
	args := []string{"list", "--json"}
	if status != "" {
		args = append(args, "--status", status)
	}
	out, err := c.run(ctx, db, args...)
	if err != nil {
		return nil, err
	}
	return c.decodeIssues(out, db)
}

// ByLabel returns the issues carrying a label, exactly.
//
// Used to answer "did I already create this?" before creating it. The label
// carries the id of the thing outside Beads that caused the bead, so the graph
// itself is the record of what has been published -- rather than a control-plane
// row that may not have been written if the process died between the two.
//
// Not on the Client interface: it exists for that one question, and an
// interface method every implementation must carry for one caller is a cost
// paid everywhere.
func (c *CLIClient) ByLabel(ctx context.Context, db DatabaseRef, label string) ([]Issue, error) {
	if strings.TrimSpace(label) == "" {
		return nil, errors.New("a label lookup needs a label")
	}
	out, err := c.run(ctx, db, "list", "--label", label, "--json")
	if err != nil {
		return nil, err
	}
	return c.decodeIssues(out, db)
}

// Get returns one issue.
func (c *CLIClient) Get(ctx context.Context, ref domain.WorkRef) (Issue, error) {
	if err := ref.Validate(); err != nil {
		return Issue{}, err
	}
	db := DatabaseRef{ID: ref.BeadsDatabaseID, CellID: ref.ExecutionCellID, Path: c.pathFor(ref.BeadsDatabaseID)}

	out, err := c.run(ctx, db, "show", ref.BeadID, "--json")
	if err != nil {
		return Issue{}, err
	}

	// `bd show --json` emits an ARRAY, even for a single bead. Assuming an
	// object here was wrong, and only the contract test against the real binary
	// caught it — which is the reason those tests exist.
	var raw []rawIssue
	if err := json.Unmarshal(bytes.TrimSpace(out), &raw); err != nil {
		return Issue{}, fmt.Errorf("decoding bd show: %w", err)
	}
	if len(raw) == 0 || raw[0].ID == "" {
		return Issue{}, fmt.Errorf("no such bead: %s", ref.BeadID)
	}
	return raw[0].toIssue(db, c.OrganisationID), nil
}

// Create adds a work item and returns its stable reference.
func (c *CLIClient) Create(ctx context.Context, db DatabaseRef, issue Issue) (domain.WorkRef, error) {
	if strings.TrimSpace(issue.Title) == "" {
		return domain.WorkRef{}, errors.New("a work item needs a title")
	}

	args := []string{"create", issue.Title, "--silent"}
	if issue.Type != "" {
		args = append(args, "--type", issue.Type)
	}
	if issue.Description != "" {
		args = append(args, "--description", issue.Description)
	}
	if issue.Priority > 0 {
		args = append(args, "--priority", strconv.Itoa(issue.Priority))
	}
	if len(issue.Labels) > 0 {
		args = append(args, "--labels", strings.Join(issue.Labels, ","))
	}
	if issue.Assignee != "" {
		args = append(args, "--assignee", issue.Assignee)
	}
	if !issue.Due.IsZero() {
		args = append(args, "--due", issue.Due.Format("2006-01-02"))
	}
	if issue.ExternalRef != "" {
		args = append(args, "--external-ref", issue.ExternalRef)
	}

	out, err := c.run(ctx, db, args...)
	if err != nil {
		return domain.WorkRef{}, err
	}

	id := strings.TrimSpace(string(out))
	if id == "" {
		return domain.WorkRef{}, errors.New("bd create returned no id")
	}
	return domain.WorkRef{
		OrganisationID:  c.OrganisationID,
		ExecutionCellID: db.CellID,
		BeadsDatabaseID: db.ID,
		BeadID:          id,
	}, nil
}

// Update changes an existing work item.
func (c *CLIClient) Update(ctx context.Context, ref domain.WorkRef, issue Issue) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	db := DatabaseRef{ID: ref.BeadsDatabaseID, CellID: ref.ExecutionCellID, Path: c.pathFor(ref.BeadsDatabaseID)}

	args := []string{"update", ref.BeadID}
	if issue.Status != "" {
		args = append(args, "--status", issue.Status)
	}
	if issue.Title != "" {
		args = append(args, "--title", issue.Title)
	}
	if issue.Assignee != "" {
		args = append(args, "--assignee", issue.Assignee)
	}
	if len(args) == 2 {
		return errors.New("update was asked to change nothing")
	}

	_, err := c.run(ctx, db, args...)
	return err
}

// Close closes work, and refuses to do so without evidence.
//
// "Close only with evidence" is a governance rule, not a nicety (plan section
// 24.1): a bead closed without it asserts an outcome nobody can check.
func (c *CLIClient) Close(ctx context.Context, ref domain.WorkRef, evidence string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(evidence) == "" {
		return errors.New("work may be closed only with evidence")
	}
	db := DatabaseRef{ID: ref.BeadsDatabaseID, CellID: ref.ExecutionCellID, Path: c.pathFor(ref.BeadsDatabaseID)}

	_, err := c.run(ctx, db, "close", ref.BeadID, "--reason", evidence)
	return err
}

// Link records a dependency inside one database.
//
// A cross-database link is refused rather than faked. Beads cannot express one,
// and writing it as free text would create a dependency nothing can query and
// no readiness calculation would honour (plan section 2.3).
func (c *CLIClient) Link(ctx context.Context, from, to domain.WorkRef, relation string) error {
	if err := from.Validate(); err != nil {
		return err
	}
	if err := to.Validate(); err != nil {
		return err
	}
	if from.BeadsDatabaseID != to.BeadsDatabaseID {
		return errors.New("cross-database links belong in the control plane work_links table, not in Beads")
	}
	db := DatabaseRef{ID: from.BeadsDatabaseID, CellID: from.ExecutionCellID, Path: c.pathFor(from.BeadsDatabaseID)}

	// `bd dep add <blocked> <blocker>` — the blocked item depends on the
	// blocker. Getting this the wrong way round silently inverts readiness,
	// so the argument order is asserted by a contract test.
	_, err := c.run(ctx, db, "dep", "add", from.BeadID, to.BeadID)
	return err
}

// Backup runs the Beads native backup.
//
// The JSONL export is an additional recovery aid, never the primary backup:
// it does not preserve Dolt history or state (plan section 15.3).
func (c *CLIClient) Backup(ctx context.Context, db DatabaseRef) error {
	_, err := c.run(ctx, db, "backup")
	return err
}

// Version reports the bd version in use, for the compatibility gate.
func (c *CLIClient) Version(ctx context.Context, db DatabaseRef) (string, error) {
	out, err := c.run(ctx, db, "version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), nil
}

// pathFor resolves a database's working directory.
//
// DatabasePaths is populated from the registry. A database with no recorded
// path yields an empty string, and the resulting bd invocation fails loudly
// rather than silently operating on whichever database is nearby.
func (c *CLIClient) pathFor(databaseID string) string {
	if c.DatabasePaths == nil {
		return ""
	}
	return c.DatabasePaths[databaseID]
}
