package registry

import (
	"strings"
	"testing"
)

func good() Document {
	return Document{
		Node: Node{Hostname: "workgraph-staging-execution", Environment: "staging"},
		Cells: []Cell{
			{Slug: "oss", SystemUsername: "wgcell_oss", TrustDomain: "oss",
				MaxConcurrentAgents: 2, CPUQuotaPercent: 88, MemoryLimitMB: 1398},
			{Slug: "client-nged", SystemUsername: "wgcell_client_nged", TrustDomain: "client-nged",
				MaxConcurrentAgents: 2, CPUQuotaPercent: 88, MemoryLimitMB: 1398},
		},
		Projects: []Project{
			{Slug: "portaljs-oss", Cell: "oss"},
			{Slug: "nged", Cell: "client-nged", Visibility: "restricted"},
		},
	}
}

func TestAGoodDocumentValidates(t *testing.T) {
	if err := good().Validate(); err != nil {
		t.Fatalf("the staging document was rejected: %v", err)
	}
}

// The failure this validation exists to prevent: a mistyped cell name reaching
// the database halfway through a deployment, where it surfaces as "no execution
// cell named client_nged" after the node has already been changed.
func TestAProjectPointingAtAnUndeclaredCellIsRefused(t *testing.T) {
	d := good()
	d.Projects[1].Cell = "client_nged" // underscore, not hyphen
	err := d.Validate()
	if err == nil {
		t.Fatal("a project naming a cell that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "client_nged") || !strings.Contains(err.Error(), "client-nged") {
		t.Fatalf("the error should name both the typo and the real cells: %v", err)
	}
}

func TestDuplicateCellSlugsAreRefused(t *testing.T) {
	d := good()
	d.Cells[1].Slug = "oss"
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("two cells sharing a slug were accepted: %v", err)
	}
}

func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	d := Document{
		Node:     Node{Hostname: "", Environment: "prod"},
		Cells:    []Cell{{Slug: "oss"}},
		Projects: []Project{{Slug: "p", Cell: "nope", Visibility: "secret"}},
	}
	err := d.Validate()
	if err == nil {
		t.Fatal("a document with five problems was accepted")
	}
	for _, want := range []string{
		"no hostname",         // node
		`environment "prod"`,  // not staging or production
		"no system username",  // cell
		"no trust domain",     // cell
		`names cell "nope"`,   // project
		`visibility "secret"`, // project
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the report is missing %q:\n%v", want, err)
		}
	}
}

// A shared cell is allowed and is not an error. It IS the reason spend from that
// cell cannot be attributed, so it has to be reported rather than discovered
// later as a puzzling hole in a cost report.
func TestASharedCellIsReportedButNotRejected(t *testing.T) {
	d := good()
	d.Projects = append(d.Projects, Project{Slug: "datopian-products", Cell: "oss"})

	if err := d.Validate(); err != nil {
		t.Fatalf("sharing a cell was treated as an error: %v", err)
	}
	shared := d.SharedCells()
	if got := shared["oss"]; len(got) != 2 || got[0] != "datopian-products" || got[1] != "portaljs-oss" {
		t.Fatalf("shared cells came out as %v", shared)
	}
	if _, ok := shared["client-nged"]; ok {
		t.Fatal("a cell with one project was reported as shared")
	}
}
