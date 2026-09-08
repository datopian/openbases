// Package db exposes the committed SQL migrations as an embedded filesystem.
//
// The embed lives here rather than in cmd/migrate because go:embed cannot
// reach outside its own directory, and the migrations belong next to the schema
// documentation rather than inside a command.
package db

import (
	"bufio"
	"embed"
	"strings"
)

// Migrations holds every file under migrations/. The migrate command reads them
// in lexical order, which matches the fixed-width NNNN_ prefix that
// scripts/check_migrations.sh enforces.
//
//go:embed migrations/*.sql
var Migrations embed.FS

// tenantSeeds lists the migrations that seed Datopian's own records rather than
// schema. Embedded so the list travels with the binary that acts on it: a
// manifest read from disk at run time would be absent on a deployed node, and
// absent would mean "skip nothing", which is the wrong default in the one
// direction that matters.
//
//go:embed tenant_seeds.txt
var tenantSeeds string

// TenantSeedNames returns the seed migration file names, in the order given.
//
// Comments and blank lines are ignored so the file can explain itself; the
// explanation is the point, because the list is the difference between an
// install that contains our company and one that does not.
func TenantSeedNames() []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(tenantSeeds))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}
