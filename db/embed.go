// Package db exposes the committed SQL migrations as an embedded filesystem.
//
// The embed lives here rather than in cmd/migrate because go:embed cannot
// reach outside its own directory, and the migrations belong next to the schema
// documentation rather than inside a command.
package db

import "embed"

// Migrations holds every file under migrations/. The migrate command reads them
// in lexical order, which matches the fixed-width NNNN_ prefix that
// scripts/check_migrations.sh enforces.
//
//go:embed migrations/*.sql
var Migrations embed.FS
