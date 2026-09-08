package gastown

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/datopian/openbases/internal/domain"
	"github.com/datopian/openbases/internal/runner"
)

// DirectRunner drives agents without Gas Town (ADR-0023).
//
// It sits beside CLIAdapter rather than replacing it, which is what the
// anti-corruption layer was for: this package's doc comment says the boundary
// "makes a later move to Gas City or another orchestrator possible without
// rewriting the product model", and this is that move, arriving earlier and for
// a duller reason than expected — Gas Town cannot start an agent on a machine
// with no tmux sessions, and every node is such a machine.
//
// The work of a dispatch is a plan (internal/runner) and a process. Planning is
// done here so a caller can be refused before anything is spent; the process is
// started by wg-runner ON the execution node, because that is where the cell,
// its slice and its credentials are.
type DirectRunner struct {
	// Exec starts a planned run and reports how it ended. Injected so the
	// decision path can be tested without a node, and so the transport — today
	// a command on the node, later an agentd job — is not baked in here.
	Exec func(ctx context.Context, cell CellRef, plan runner.Plan) (AgentRef, error)

	// Deadline bounds a run when the request does not say.
	Deadline time.Duration

	// GatewayToken authorises requests through the AI Gateway. Empty means every
	// dispatch is refused, which is deliberate: without it a run does not fail,
	// it succeeds straight against the provider, untagged and outside every
	// budget.
	GatewayToken string
}

// Dispatch plans a run and starts it.
func (d *DirectRunner) Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error) {
	// The capability check comes first and is unconditional, exactly as in
	// CLIAdapter. A second implementation of an interface is where an invariant
	// quietly stops being enforced, so it is repeated rather than inherited.
	if req.Capability == "" {
		return DispatchResult{}, errors.New("dispatch requires a signed capability document")
	}
	if err := req.Work.Validate(); err != nil {
		return DispatchResult{}, err
	}
	if d.Exec == nil {
		return DispatchResult{}, errors.New("no execution transport configured")
	}

	deadline := d.Deadline
	if deadline == 0 {
		deadline = 15 * time.Minute
	}

	plan, err := runner.New(runner.Spec{
		Bead:         req.Work.BeadID,
		Cell:         req.Cell.ID,
		CellRoot:     "/srv/cells/" + req.Cell.ID,
		Instructions: instructionsFor(req.Work),
		GatewayToken: d.GatewayToken,
		Deadline:     deadline,
	})
	if err != nil {
		// Refused, not accepted-then-failed. The caller can tell a bad request
		// from work that did not succeed, and nothing has been spent.
		return DispatchResult{Accepted: false, Reason: err.Error()}, nil
	}

	ref, err := d.Exec(ctx, req.Cell, plan)
	if err != nil {
		return DispatchResult{Accepted: false, Reason: err.Error()}, err
	}
	return DispatchResult{Ref: ref, Accepted: true}, nil
}

// instructionsFor is the prompt a dispatched agent receives.
//
// Deliberately minimal and deliberately here rather than in internal/runner:
// what an agent is asked to do is a product decision, and the planner should not
// have opinions about it.
func instructionsFor(work domain.WorkRef) string {
	return fmt.Sprintf(
		"Work the bead %s. Read it first, do what it asks, and close it when the work is done "+
			"and not before. If you cannot complete it, leave it open and say why in a comment.",
		work.BeadID)
}

// The rest of the interface is not part of this first cut, and says so rather
// than pretending. Health, ListAgents and GetActivity describe a fleet of
// long-lived agents; this runner starts one agent for one bead and it exits.
// They become meaningful when there is something to be running (wg-8el).
func (d *DirectRunner) Health(context.Context, CellRef) (Health, error) {
	return Health{}, ErrNotImplemented
}
func (d *DirectRunner) ListAgents(context.Context, CellRef) ([]AgentState, error) {
	return nil, ErrNotImplemented
}
func (d *DirectRunner) Pause(context.Context, AgentRef) error  { return ErrNotImplemented }
func (d *DirectRunner) Resume(context.Context, AgentRef) error { return ErrNotImplemented }
func (d *DirectRunner) Nudge(context.Context, AgentRef, string) error {
	// A nudge is a message to a session that is waiting. This runner's agent is
	// never waiting: it is given its instructions once and runs to completion or
	// to its deadline.
	return ErrNotImplemented
}
func (d *DirectRunner) GetActivity(context.Context, ActivityFilter) ([]ActivityEvent, error) {
	return nil, ErrNotImplemented
}
func (d *DirectRunner) GetConvoy(context.Context, domain.WorkRef) (ConvoyState, error) {
	return ConvoyState{}, ErrNotImplemented
}
