// Package gastown is the anti-corruption layer in front of Gas Town.
//
// Nothing outside this package shells out to `gt`. Keeping the boundary makes a
// later move to Gas City or another orchestrator possible without rewriting the
// product model (plan section 7.5).
package gastown

import (
	"context"
	"errors"
	"time"

	"github.com/datopian/openbases/internal/domain"
)

// ErrNotImplemented marks an operation that WP-E2 has not yet delivered.
var ErrNotImplemented = errors.New("gastown adapter not implemented (WP-E2)")

// CellRef identifies an execution cell — the security and runtime boundary.
type CellRef struct {
	ID   string
	Node string
}

// AgentRef identifies a running agent session within a cell.
type AgentRef struct {
	Cell    CellRef
	AgentID string
}

// Health is the observed state of a cell.
type Health struct {
	Cell      CellRef
	Healthy   bool
	Reason    string
	CheckedAt time.Time
	Versions  Versions
}

// Versions records the exact toolchain a cell is running. Independent upgrades
// are blocked; the matrix moves together (plan section 7.6).
type Versions struct {
	GasTown string
	Beads   string
	Dolt    string
}

// AgentState is a projected view of one agent session.
type AgentState struct {
	Ref       AgentRef
	Work      domain.WorkRef
	Status    string // starting | running | waiting | stalled | completed | failed
	Since     time.Time
	LastEvent time.Time
}

// DispatchRequest asks the orchestrator to start work on a bead.
type DispatchRequest struct {
	Cell CellRef
	Work domain.WorkRef
	// Capability is the signed, expiring authority for this invocation:
	// actor, project, repositories, allowed tools, action classes, budget and
	// expiry (plan section 8.3). Dispatch without one must fail.
	Capability string
}

// DispatchResult reports the outcome of a dispatch.
type DispatchResult struct {
	Ref      AgentRef
	Accepted bool
	Reason   string
}

// ActivityFilter selects activity events.
type ActivityFilter struct {
	Cell  CellRef
	Since time.Time
	Limit int
}

// ActivityEvent is one observable thing an agent did.
type ActivityEvent struct {
	Ref     AgentRef
	At      time.Time
	Type    string
	Message string
}

// ConvoyState is the aggregate state of a batch of related work.
type ConvoyState struct {
	Work     domain.WorkRef
	Total    int
	Complete int
	Failed   int
}

// Orchestrator is the contract the rest of the application depends on
// (plan section 7.5).
type Orchestrator interface {
	Health(ctx context.Context, cell CellRef) (Health, error)
	ListAgents(ctx context.Context, cell CellRef) ([]AgentState, error)
	Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error)
	Pause(ctx context.Context, ref AgentRef) error
	Resume(ctx context.Context, ref AgentRef) error
	Nudge(ctx context.Context, ref AgentRef, message string) error
	GetActivity(ctx context.Context, filter ActivityFilter) ([]ActivityEvent, error)
	GetConvoy(ctx context.Context, ref domain.WorkRef) (ConvoyState, error)
}

// CLIAdapter will drive Gas Town through the pinned `gt` binary. It is declared
// here so that the interface has a named production implementation from the
// start; the operations land in WP-E2.
type CLIAdapter struct {
	// Binary is the absolute path to the pinned `gt` executable, verified
	// against versions.lock before use.
	Binary string
}

var _ Orchestrator = (*CLIAdapter)(nil)

func (c *CLIAdapter) Health(context.Context, CellRef) (Health, error) {
	return Health{}, ErrNotImplemented
}
func (c *CLIAdapter) ListAgents(context.Context, CellRef) ([]AgentState, error) {
	return nil, ErrNotImplemented
}

// Dispatch refuses a request without a signed capability even before the
// adapter is implemented, so the invariant cannot be lost during WP-E2.
func (c *CLIAdapter) Dispatch(_ context.Context, req DispatchRequest) (DispatchResult, error) {
	if req.Capability == "" {
		return DispatchResult{}, errors.New("dispatch requires a signed capability document")
	}
	if err := req.Work.Validate(); err != nil {
		return DispatchResult{}, err
	}
	return DispatchResult{}, ErrNotImplemented
}
func (c *CLIAdapter) Pause(context.Context, AgentRef) error  { return ErrNotImplemented }
func (c *CLIAdapter) Resume(context.Context, AgentRef) error { return ErrNotImplemented }
func (c *CLIAdapter) Nudge(context.Context, AgentRef, string) error {
	return ErrNotImplemented
}
func (c *CLIAdapter) GetActivity(context.Context, ActivityFilter) ([]ActivityEvent, error) {
	return nil, ErrNotImplemented
}
func (c *CLIAdapter) GetConvoy(context.Context, domain.WorkRef) (ConvoyState, error) {
	return ConvoyState{}, ErrNotImplemented
}
