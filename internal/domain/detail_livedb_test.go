package domain

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// The project detail reads back through the real driver.
//
// This test exists because of a specific failure it would have caught and
// nothing else did. The blocked_by and blocking columns were written as
// Postgres arrays and scanned into Go []string. That COMPILES, the SQL is
// correct in psql, and the integration test asserting the edges passed -- but
// this driver hands an array back as its text form, so at run time the scan
// failed with "unsupported Scan, storing driver.Value type string into type
// *[]string" and the project page answered 500.
//
// Every test the change had was on the wrong side of the driver: unit tests
// with no database, and SQL tests with no Go. The gap between them is exactly
// where a type mapping lives.
//
// Skipped without WG_TEST_DSN, so a laptop with no database still runs the
// suite. CI's database job sets it, which is where this has to run.
func TestProjectDetailReadsListsThroughTheDriver(t *testing.T) {
	dsn := os.Getenv("WG_TEST_DSN")
	if dsn == "" {
		t.Skip("set WG_TEST_DSN to run this against a real PostgreSQL")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("connecting: %v", err)
	}

	// Fixture, created as the owner deliberately: the property under test is
	// how the DRIVER maps a value, not who may see the row, and visibility is
	// covered by the SQL tests.
	//
	// Not in a transaction, because Store holds its own *sql.DB and there is
	// nowhere to inject one -- so it is cleaned up explicitly instead. The
	// project cascade takes the beads, the links and the memberships with it.
	var org, user, backup, proj, cell, node, graph string
	must := func(q string, args ...any) string {
		var id string
		if err := db.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", strings.SplitN(q, "\n", 2)[0], err)
		}
		return id
	}
	org = must(`SELECT id::text FROM organisations ORDER BY created_at LIMIT 1`)
	node = must(`INSERT INTO execution_nodes (hostname, environment) VALUES
	               ('driver-probe.invalid','staging')
	             ON CONFLICT (hostname) DO UPDATE SET environment = EXCLUDED.environment
	             RETURNING id::text`)
	cell = must(`INSERT INTO execution_cells (execution_node_id, slug, system_username, trust_domain)
	             VALUES ($1::uuid,'drv-cell','wgcell_drv','oss') RETURNING id::text`, node)
	user = must(`INSERT INTO users (organisation_id, display_name, primary_email)
	             VALUES ($1::uuid,'Drv Owner','drv-owner@example.invalid') RETURNING id::text`, org)
	backup = must(`INSERT INTO users (organisation_id, display_name, primary_email)
	               VALUES ($1::uuid,'Drv Backup','drv-backup@example.invalid') RETURNING id::text`, org)
	proj = must(`INSERT INTO projects (organisation_id, slug, name, primary_owner_id,
	                                   backup_owner_id, execution_cell_id)
	             VALUES ($1::uuid,'drv-proj','Driver Project',$2::uuid,$3::uuid,$4::uuid)
	             RETURNING id::text`, org, user, backup, cell)
	graph = must(`INSERT INTO beads_databases (organisation_id, execution_cell_id, name, scope, project_id)
	              VALUES ($1::uuid,$2::uuid,'drv-graph','project',$3::uuid) RETURNING id::text`,
		org, cell, proj)

	// The owner is a member, or ProjectBySlug refuses before the work query
	// is reached and this would pass for the wrong reason.
	t.Cleanup(func() {
		// Order matters: the graph references the cell, and the cell the node.
		for _, q := range []string{
			`DELETE FROM projects WHERE slug = 'drv-proj'`,
			`DELETE FROM beads_databases WHERE name = 'drv-graph'`,
			`DELETE FROM execution_cells WHERE slug = 'drv-cell'`,
			`DELETE FROM execution_nodes WHERE hostname = 'driver-probe.invalid'`,
			`DELETE FROM users WHERE primary_email IN ('drv-owner@example.invalid','drv-backup@example.invalid')`,
		} {
			if _, err := db.ExecContext(context.Background(), q); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	})

	if _, err := db.ExecContext(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role_name)
		 VALUES ($1::uuid,$2::uuid,'contributor') ON CONFLICT DO NOTHING`, proj, user); err != nil {
		t.Fatalf("membership: %v", err)
	}

	blocker := must(`INSERT INTO work_refs (organisation_id, beads_database_id, execution_cell_id,
	                                        bead_id, title, kind, status, project_id)
	                 VALUES ($1::uuid,$2::uuid,$3::uuid,'drv-1','the blocker','task','open',$4::uuid)
	                 RETURNING id::text`, org, graph, cell, proj)
	blocked := must(`INSERT INTO work_refs (organisation_id, beads_database_id, execution_cell_id,
	                                        bead_id, title, kind, status, project_id)
	                 VALUES ($1::uuid,$2::uuid,$3::uuid,'drv-2','the blocked','task','open',$4::uuid)
	                 RETURNING id::text`, org, graph, cell, proj)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO work_links (from_work_ref, to_work_ref, relation)
		 VALUES ($1::uuid,$2::uuid,'blocks')`, blocker, blocked); err != nil {
		t.Fatalf("link: %v", err)
	}

	// The real read path, through the real driver. This is the line the bug
	// lived on.
	detail, err := NewStore(db).ProjectDetailBySlug(ctx, user, "drv-proj")
	if err != nil {
		t.Fatalf("reading the detail: %v", err)
	}

	var sawBlocked, sawBlocker bool
	for _, w := range detail.Work {
		switch w.Bead {
		case "drv-2":
			sawBlocked = true
			if len(w.BlockedBy) != 1 || w.BlockedBy[0] != "drv-1" {
				t.Errorf("drv-2 blocked_by is %v, wanted [drv-1]", w.BlockedBy)
			}
			if len(w.Blocking) != 0 {
				t.Errorf("drv-2 blocking is %v, wanted empty", w.Blocking)
			}
		case "drv-1":
			sawBlocker = true
			// The direction again, from the Go side this time.
			if len(w.Blocking) != 1 || w.Blocking[0] != "drv-2" {
				t.Errorf("drv-1 blocking is %v, wanted [drv-2]", w.Blocking)
			}
			if len(w.BlockedBy) != 0 {
				t.Errorf("drv-1 blocked_by is %v, wanted empty; the direction is inverted", w.BlockedBy)
			}
		}
	}
	if !sawBlocked || !sawBlocker {
		t.Fatalf("the fixture beads were not returned: blocked=%v blocker=%v", sawBlocked, sawBlocker)
	}
}
