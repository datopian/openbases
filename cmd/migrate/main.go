// Command migrate applies database migrations in order and records what it did.
//
// Migrations are forward-only and each runs in its own transaction, so a failure
// leaves the schema at the last complete migration rather than half-applied
// (db/migrations/README.md).
//
// It is deliberately small. A migration tool that can roll back encourages
// destructive down-migrations, and plan section 16.2 requires backwards
// compatible changes plus a pre-deploy backup instead.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/workgraph/db"
)

type migration struct {
	name     string
	sql      string
	checksum string
}

func main() {
	var (
		dsn    = flag.String("dsn", os.Getenv("WG_DATABASE_URL"), "PostgreSQL connection string")
		dryRun = flag.Bool("dry-run", false, "report what would be applied without applying it")
		verify = flag.Bool("verify", false, "check applied migrations still match their recorded checksums, then exit")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *dsn == "" {
		log.Error("no connection string; set WG_DATABASE_URL or pass -dsn")
		os.Exit(1)
	}

	if err := run(context.Background(), log, *dsn, *dryRun, *verify); err != nil {
		log.Error("migration failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, dsn string, dryRun, verifyOnly bool) error {
	all, err := load()
	if err != nil {
		return fmt.Errorf("loading migrations: %w", err)
	}
	log.Info("migrations found", "count", len(all))

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connecting: %w", err)
	}

	if err := ensureTable(ctx, db); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	applied, err := appliedSet(ctx, db)
	if err != nil {
		return fmt.Errorf("reading applied migrations: %w", err)
	}

	// A migration that changed after it was applied means the schema on this
	// database is not what the repository describes. That is a correctness
	// problem, not a style one, so it stops the run.
	for _, m := range all {
		if prev, ok := applied[m.name]; ok && prev != m.checksum {
			return fmt.Errorf(
				"%s was applied with checksum %s but now hashes to %s; "+
					"an applied migration must never be edited — write a new one",
				m.name, prev[:12], m.checksum[:12])
		}
	}
	if verifyOnly {
		log.Info("all applied migrations match their recorded checksums")
		return nil
	}

	pending := make([]migration, 0, len(all))
	for _, m := range all {
		if _, ok := applied[m.name]; !ok {
			pending = append(pending, m)
		}
	}

	if len(pending) == 0 {
		log.Info("database is up to date", "applied", len(applied))
		return nil
	}

	for _, m := range pending {
		if dryRun {
			log.Info("would apply", "migration", m.name)
			continue
		}
		start := time.Now()
		if err := apply(ctx, db, m); err != nil {
			return fmt.Errorf("applying %s: %w", m.name, err)
		}
		log.Info("applied", "migration", m.name, "duration", time.Since(start).Round(time.Millisecond))
	}
	return nil
}

func load() ([]migration, error) {
	entries, err := fs.ReadDir(db.Migrations, "migrations")
	if err != nil {
		return nil, err
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := db.Migrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			name:     e.Name(),
			sql:      string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}
	// Lexical order matches the NNNN_ prefix, which is why the prefix is fixed
	// width and checked by scripts/check_migrations.sh.
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func ensureTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name        text PRIMARY KEY,
			checksum    text NOT NULL,
			applied_at  timestamptz NOT NULL DEFAULT now()
		)`)
	return err
}

func appliedSet(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, checksum FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var name, sum string
		if err := rows.Scan(&name, &sum); err != nil {
			return nil, err
		}
		out[name] = sum
	}
	return out, rows.Err()
}

// apply runs one migration and records it in the same transaction, so the
// record and the change cannot disagree.
//
// Each migration file carries its own BEGIN/COMMIT, checked by
// scripts/check_migrations.sh. Postgres treats nested BEGIN as a no-op with a
// warning, and the outer transaction is what actually provides atomicity here.
func apply(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (name, checksum) VALUES ($1, $2)`,
		m.name, m.checksum); err != nil {
		return err
	}
	return tx.Commit()
}
