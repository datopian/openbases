// Package beads adapts the `bd` CLI.
//
// Beads is used without forking. Writes go through `bd` commands only; the
// control plane never mutates Dolt tables directly. Reads prefer `bd ... --json`
// for contract stability, and any read-only SQL stays behind this adapter with
// version-specific tests (plan section 7.1).
package beads

import (
	"context"
	"errors"
	"time"

	"github.com/datopian/workgraph/internal/domain"
)

// ErrNotImplemented marks an operation that WP-D2 has not yet delivered.
var ErrNotImplemented = errors.New("beads adapter not implemented (WP-D2)")

// DatabaseRef identifies one Beads database in the registry.
type DatabaseRef struct {
	ID       string
	Name     string
	CellID   string
	Scope    domain.Scope
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
}

// CLIClient drives the pinned `bd` binary.
type CLIClient struct {
	// Binary is the absolute path to the pinned `bd` executable, verified
	// against versions.lock before use.
	Binary string
	// Record receives one entry per invocation for the audit log.
	Record func(CommandRecord)
}

var _ Client = (*CLIClient)(nil)

func (c *CLIClient) Ready(context.Context, DatabaseRef) ([]Issue, error) {
	return nil, ErrNotImplemented
}
func (c *CLIClient) Get(context.Context, domain.WorkRef) (Issue, error) {
	return Issue{}, ErrNotImplemented
}
func (c *CLIClient) Create(context.Context, DatabaseRef, Issue) (domain.WorkRef, error) {
	return domain.WorkRef{}, ErrNotImplemented
}
func (c *CLIClient) Update(context.Context, domain.WorkRef, Issue) error { return ErrNotImplemented }

// Close refuses to close work without evidence even before the adapter is
// implemented: "close only with evidence" is a governance rule, not a nicety
// (plan section 24.1).
func (c *CLIClient) Close(_ context.Context, ref domain.WorkRef, evidence string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if evidence == "" {
		return errors.New("work may be closed only with evidence")
	}
	return ErrNotImplemented
}

// Link refuses a cross-database link. Beads cannot express it, and faking it
// with a text reference would create an invisible, unqueryable dependency.
func (c *CLIClient) Link(_ context.Context, from, to domain.WorkRef, _ string) error {
	if from.BeadsDatabaseID != to.BeadsDatabaseID {
		return errors.New("cross-database links belong in the control plane work_links table, not in Beads")
	}
	return ErrNotImplemented
}
func (c *CLIClient) Backup(context.Context, DatabaseRef) error { return ErrNotImplemented }
