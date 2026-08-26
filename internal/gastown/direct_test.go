package gastown

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/datopian/workgraph/internal/domain"
	"github.com/datopian/workgraph/internal/runner"
)

func work() domain.WorkRef {
	return domain.WorkRef{
		OrganisationID: "org", ExecutionCellID: "oss",
		BeadsDatabaseID: "db", BeadID: "wg-8el",
	}
}

func runnerUnderTest(t *testing.T) (*DirectRunner, *runner.Plan) {
	t.Helper()
	var seen runner.Plan
	d := &DirectRunner{
		GatewayToken: "tok",
		Deadline:     10 * time.Minute,
		Exec: func(_ context.Context, cell CellRef, p runner.Plan) (AgentRef, error) {
			seen = p
			return AgentRef{Cell: cell, AgentID: "agent-1"}, nil
		},
	}
	return d, &seen
}

// A second implementation of an interface is exactly where an invariant quietly
// stops being enforced, so the capability check is asserted here as well as on
// CLIAdapter.
func TestDispatchWithoutACapabilityIsRefused(t *testing.T) {
	d, _ := runnerUnderTest(t)
	_, err := d.Dispatch(context.Background(), DispatchRequest{Cell: CellRef{ID: "oss"}, Work: work()})
	if err == nil {
		t.Fatal("a dispatch with no signed capability was accepted")
	}
	if !strings.Contains(err.Error(), "capability") {
		t.Errorf("the refusal should say what is missing: %v", err)
	}
}

func TestDispatchValidatesTheWorkReference(t *testing.T) {
	d, _ := runnerUnderTest(t)
	incomplete := work()
	incomplete.BeadID = ""
	if _, err := d.Dispatch(context.Background(), DispatchRequest{
		Cell: CellRef{ID: "oss"}, Work: incomplete, Capability: "signed",
	}); err == nil {
		t.Fatal("a dispatch with an incomplete work reference was accepted")
	}
}

// Without the gateway token a run does not fail — it succeeds straight against
// the provider, untagged and outside every budget. Refusing is the point.
func TestDispatchWithoutAGatewayTokenIsRefusedBeforeAnythingIsSpent(t *testing.T) {
	d, seen := runnerUnderTest(t)
	d.GatewayToken = ""

	res, err := d.Dispatch(context.Background(), DispatchRequest{
		Cell: CellRef{ID: "oss"}, Work: work(), Capability: "signed",
	})
	if err != nil {
		t.Fatalf("a refusal should be an answer, not an error: %v", err)
	}
	if res.Accepted {
		t.Fatal("dispatched without a gateway token")
	}
	if seen.RunDir != "" {
		t.Fatal("the run was started despite being refused")
	}
	if !strings.Contains(res.Reason, "budget") {
		t.Errorf("the reason should say what is at stake: %s", res.Reason)
	}
}

func TestAnAcceptedDispatchCarriesItsBeadIntoThePlan(t *testing.T) {
	d, seen := runnerUnderTest(t)
	res, err := d.Dispatch(context.Background(), DispatchRequest{
		Cell: CellRef{ID: "oss"}, Work: work(), Capability: "signed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted {
		t.Fatalf("refused: %s", res.Reason)
	}
	if seen.Metadata["bead"] != "wg-8el" || seen.Metadata["cell"] != "oss" {
		t.Fatalf("the plan lost its attribution: %v", seen.Metadata)
	}
	if !strings.Contains(seen.RunDir, "wg-8el") {
		t.Errorf("the run directory should name its bead: %s", seen.RunDir)
	}
	if res.Ref.AgentID == "" {
		t.Error("an accepted dispatch should name the agent it started")
	}
}

// A dispatch that cannot reach a node must not report success.
func TestDispatchWithNoTransportIsRefused(t *testing.T) {
	d := &DirectRunner{GatewayToken: "tok"}
	if _, err := d.Dispatch(context.Background(), DispatchRequest{
		Cell: CellRef{ID: "oss"}, Work: work(), Capability: "signed",
	}); err == nil {
		t.Fatal("dispatched with no way to start anything")
	}
}

// The instructions must name the bead and must not invite the agent to close it
// regardless of outcome — a bead closed without the work done is worse than one
// left open.
func TestInstructionsNameTheBeadAndDoNotInviteAFalseClose(t *testing.T) {
	got := instructionsFor(work())
	if !strings.Contains(got, "wg-8el") {
		t.Errorf("the instructions do not name the bead: %s", got)
	}
	if !strings.Contains(got, "not before") {
		t.Errorf("the instructions should say when NOT to close: %s", got)
	}
}

// DirectRunner must satisfy the interface it is replacing an implementation of.
var _ Orchestrator = (*DirectRunner)(nil)
