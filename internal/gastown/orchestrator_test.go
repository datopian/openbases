package gastown

import (
	"context"
	"strings"
	"testing"

	"github.com/datopian/workgraph/internal/domain"
)

func TestDispatch_RequiresSignedCapability(t *testing.T) {
	a := &CLIAdapter{Binary: "/usr/local/bin/gt"}
	work := domain.WorkRef{OrganisationID: "o", ExecutionCellID: "c", BeadsDatabaseID: "d", BeadID: "wg-1"}

	_, err := a.Dispatch(context.Background(), DispatchRequest{Work: work})
	if err == nil || !strings.Contains(err.Error(), "capability") {
		t.Fatalf("dispatch without a capability must be refused, got %v", err)
	}
}

func TestDispatch_RequiresCompleteWorkRef(t *testing.T) {
	a := &CLIAdapter{}
	_, err := a.Dispatch(context.Background(), DispatchRequest{
		Work:       domain.WorkRef{BeadID: "wg-1"},
		Capability: "signed-capability",
	})
	if err == nil {
		t.Fatal("dispatch must reject a bead ID without its identity tuple")
	}
}
