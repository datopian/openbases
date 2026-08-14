package beads

import (
	"context"
	"strings"
	"testing"

	"github.com/datopian/workgraph/internal/domain"
)

func ref(db, id string) domain.WorkRef {
	return domain.WorkRef{OrganisationID: "o", ExecutionCellID: "c", BeadsDatabaseID: db, BeadID: id}
}

// Closing without evidence is refused before bd is invoked at all, so the rule
// holds even if the binary is missing.
func TestClose_RequiresEvidence(t *testing.T) {
	c := &CLIClient{}
	if err := c.Close(context.Background(), ref("db1", "wg-1"), ""); err == nil {
		t.Fatal("closing work without evidence must be refused")
	}
	if err := c.Close(context.Background(), ref("db1", "wg-1"), "   "); err == nil {
		t.Fatal("whitespace is not evidence")
	}
}

// A cross-database link is refused rather than faked: Beads cannot express one,
// and writing it as free text would create a dependency nothing can query.
func TestLink_RefusesCrossDatabase(t *testing.T) {
	c := &CLIClient{}
	err := c.Link(context.Background(), ref("db1", "wg-1"), ref("db2", "px-9"), "blocks")
	if err == nil || !strings.Contains(err.Error(), "work_links") {
		t.Fatalf("a cross-database link must be refused and redirected to work_links, got %v", err)
	}
}

// An incomplete reference must be rejected before any command runs. A bead ID
// alone is not an identity: project prefixes collide across databases.
func TestOperationsRejectIncompleteReferences(t *testing.T) {
	c := &CLIClient{}
	partial := domain.WorkRef{BeadID: "wg-1"}
	ctx := context.Background()

	if _, err := c.Get(ctx, partial); err == nil {
		t.Error("Get accepted a bead ID with no identity tuple")
	}
	if err := c.Update(ctx, partial, Issue{Status: "closed"}); err == nil {
		t.Error("Update accepted a bead ID with no identity tuple")
	}
	if err := c.Close(ctx, partial, "evidence"); err == nil {
		t.Error("Close accepted a bead ID with no identity tuple")
	}
}

func TestCreateRequiresATitle(t *testing.T) {
	c := &CLIClient{}
	if _, err := c.Create(context.Background(), DatabaseRef{ID: "db1"}, Issue{Title: "  "}); err == nil {
		t.Fatal("a work item with no title must be refused")
	}
}

// An update that changes nothing is a no-op that would still write a Dolt
// commit, so it is refused rather than silently recorded.
func TestUpdateRefusesAnEmptyChange(t *testing.T) {
	c := &CLIClient{}
	if err := c.Update(context.Background(), ref("db1", "wg-1"), Issue{}); err == nil {
		t.Fatal("an update that changes nothing must be refused")
	}
}
