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
	ddl := regexp.MustCompile(`(?im)^\s*(CREATE|ALTER|DROP)\s+(TABLE|FUNCTION|VIEW|TYPE|INDEX|POLICY|TRIGGER|EXTENSION|ROLE)\b`)

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

// statementsOnly returns the SQL with dollar-quoted bodies removed, leaving
// what the migration actually executes when applied.
func statementsOnly(sql string) string {
	parts := dollarQuoted.Split(sql, -1)
	var b strings.Builder
	for i := 0; i < len(parts); i += 2 { // outside the quoted pairs
		b.WriteString(parts[i])
		b.WriteString("\n")
	}
	return b.String()
}
