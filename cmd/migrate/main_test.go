package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/datopian/openbases/db"
)

// The committed migrations must load, be ordered, and hash stably. A migration
// that cannot be read is a deployment that fails at the worst moment.
func TestLoadMigrations(t *testing.T) {
	all, err := load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no migrations embedded; the deploy would silently do nothing")
	}

	for i, m := range all {
		if !strings.HasSuffix(m.name, ".sql") {
			t.Errorf("%s is not a .sql file", m.name)
		}
		if len(m.checksum) != 64 {
			t.Errorf("%s has a malformed checksum %q", m.name, m.checksum)
		}
		if m.sql == "" {
			t.Errorf("%s is empty", m.name)
		}
		// Lexical order is what makes the NNNN_ prefix meaningful.
		if i > 0 && all[i-1].name >= m.name {
			t.Errorf("migrations out of order: %s before %s", all[i-1].name, m.name)
		}
	}
}

// The checksum must depend on content, or an edited migration would pass the
// tamper check that exists to catch exactly that.
func TestChecksumIsContentAddressed(t *testing.T) {
	all, err := load()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, m := range all {
		if prev, dup := seen[m.checksum]; dup {
			t.Errorf("%s and %s hash identically; content-addressing is broken", prev, m.name)
		}
		seen[m.checksum] = m.name
	}
}

// The seed manifest names real migrations. A name matching nothing would skip
// nothing, and -fresh would then install Datopian's records into somebody
// else's deployment while reporting success -- the exact silence this whole
// mechanism exists to prevent.
func TestTenantSeedManifestNamesRealMigrations(t *testing.T) {
	all, err := load()
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, m := range all {
		known[m.name] = true
	}

	seeds := db.TenantSeedNames()
	if len(seeds) == 0 {
		t.Fatal("the manifest is empty; -fresh would skip nothing")
	}
	for _, name := range seeds {
		if !known[name] {
			t.Errorf("db/tenant_seeds.txt names %q, which is not a migration", name)
		}
	}
}

// A migration in the manifest must contain no DDL, because -fresh does not run
// it. Add one that also creates a table and every fresh install is missing that
// table -- and the failure lands on the person installing, not on us.
func TestTenantSeedsCarryNoSchema(t *testing.T) {
	all, err := load()
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]string{}
	for _, m := range all {
		body[m.name] = m.sql
	}

	// Matched on statement starts only. "CREATE" appears in prose and inside
	// function bodies that a seed may legitimately call.
	// `OR REPLACE` is load-bearing. Without it this matched only
	// "CREATE FUNCTION", so 0027_execution_registry.sql -- four
	// CREATE OR REPLACE FUNCTION definitions and no seed at all -- passed this
	// test while in the manifest. Every fresh install was therefore missing
	// system_register_execution_node, system_register_execution_cell,
	// system_attach_project_to_cell and system_record_usage, and came up
	// looking clean because nothing checked that a function existed.
	ddl := regexp.MustCompile(`(?im)^\s*(CREATE|ALTER|DROP)\s+(OR\s+REPLACE\s+)?` +
		`(TABLE|FUNCTION|PROCEDURE|VIEW|MATERIALIZED|TYPE|INDEX|POLICY|TRIGGER|EXTENSION|ROLE)\b`)

	for _, name := range db.TenantSeedNames() {
		sql, ok := body[name]
		if !ok {
			continue // reported by the test above
		}
		if m := ddl.FindString(sql); m != "" {
			t.Errorf("%s is in the seed manifest but contains schema (%q); "+
				"-fresh skips it, so every fresh install would be missing that",
				name, strings.TrimSpace(m))
		}
	}
}

// No migration outside the manifest may seed a record. This is the invariant a
// fresh install rests on, and it is the one worth a test: -fresh skips exactly
// the manifest, so anything seeding elsewhere lands in every install ever made.
//
// Only STATEMENTS count, not SQL text. Seven migrations contain `INSERT INTO`
// against business tables inside `CREATE FUNCTION` bodies -- 0084 registers a
// rig, 0057 a beads graph -- and those run when the function is called, never
// when the migration is applied. A test that matched the text would report all
// seven and be weakened or ignored within a day, which is worse than no test.
// So dollar-quoted bodies are stripped first.
func TestNoMigrationOutsideTheManifestSeedsARecord(t *testing.T) {
	all, err := load()
	if err != nil {
		t.Fatal(err)
	}
	skipped := map[string]bool{}
	for _, n := range db.TenantSeedNames() {
		skipped[n] = true
	}

	// `roles` is deliberately absent: it is the permission model, so seeding it
	// is schema. 0001_core.sql does exactly that and must keep passing.
	business := regexp.MustCompile(`(?i)INSERT\s+INTO\s+(organisations|users|identities|projects|portfolios|` +
		`project_memberships|project_repositories|role_grants|event_sources|beads_databases|` +
		`work_refs|execution_nodes|execution_cells|execution_rigs|agent_profiles)\b`)

	for _, m := range all {
		if skipped[m.name] {
			continue
		}
		if hit := business.FindString(statementsOnly(m.sql)); hit != "" {
			t.Errorf("%s seeds a record (%q) and is not in db/tenant_seeds.txt, so every "+
				"fresh install would contain it. A record about the real world belongs in "+
				"the deployment's database, not in a migration.",
				m.name, strings.Join(strings.Fields(hit), " "))
		}
	}
}

// dollarQuoted matches a dollar-quoting tag: $$, $function$, $_$.
var dollarQuoted = regexp.MustCompile(`\$[a-zA-Z_][a-zA-Z_0-9]*\$|\$\$`)

// endsWithDO matches text that ends in the DO of a DO block.
var endsWithDO = regexp.MustCompile(`(?i)\bDO\s*$`)

// statementsOnly returns the SQL that runs when the migration is APPLIED.
//
// A CREATE FUNCTION body is removed: it runs when the function is called, and
// treating it as a seed reported every registration function as a violation.
//
// A `DO $$ ... $$` block is KEPT, because it executes immediately, so an insert
// inside one is a seed like any other. An earlier version stripped both, which
// made this test blind to exactly the thing it exists to catch —
// 0080_restructure_projects.sql seeds through a DO block.
func statementsOnly(sql string) string {
	var b strings.Builder
	pos := 0
	for {
		open := dollarQuoted.FindStringIndex(sql[pos:])
		if open == nil {
			b.WriteString(sql[pos:])
			return b.String()
		}
		start := pos + open[0]
		bodyAt := pos + open[1]
		close := dollarQuoted.FindStringIndex(sql[bodyAt:])
		if close == nil { // unterminated; keep the remainder rather than lose it
			b.WriteString(sql[pos:])
			return b.String()
		}
		b.WriteString(sql[pos:start])
		b.WriteString("\n")
		if endsWithDO.MatchString(sql[:start]) {
			b.WriteString(sql[bodyAt : bodyAt+close[0]])
			b.WriteString("\n")
		}
		pos = bodyAt + close[1]
	}
}
