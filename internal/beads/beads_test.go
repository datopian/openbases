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

func TestClose_RequiresEvidence(t *testing.T) {
	c := &CLIClient{}
	if err := c.Close(context.Background(), ref("db1", "wg-1"), ""); err == nil {
		t.Fatal("closing work without evidence must be refused")
	}
}

func TestLink_RefusesCrossDatabase(t *testing.T) {
	c := &CLIClient{}
	err := c.Link(context.Background(), ref("db1", "wg-1"), ref("db2", "px-9"), "blocks")
	if err == nil || !strings.Contains(err.Error(), "work_links") {
		t.Fatalf("a cross-database link must be refused and redirected to work_links, got %v", err)
	}
}

func TestLink_AllowsSameDatabase(t *testing.T) {
	c := &CLIClient{}
	err := c.Link(context.Background(), ref("db1", "wg-1"), ref("db1", "wg-2"), "blocks")
	if err == nil || err.Error() == "" {
		t.Fatal("expected the not-implemented sentinel")
	}
	if strings.Contains(err.Error(), "work_links") {
		t.Fatal("a same-database link must not be rejected as cross-database")
	}
}
