// Command wg-registry records which execution nodes and cells exist.
//
// It reads a desired-state document on stdin and upserts it. Ansible produces
// the document, because Ansible is what knows: the inventory names the nodes,
// group_vars/execution.yml declares the cells, and the execution_cell role
// derives the limits actually applied from the node's real CPU and memory.
//
// Run at deploy time rather than on a timer. Registration is not a thing that
// drifts on its own — it changes when a node or a cell changes, which is when
// Ansible runs — and a timer would be a second writer racing the deployment.
//
// Everything is idempotent, so re-running is free and a partial run is
// recoverable by running it again.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/openbases/internal/config"
	"github.com/datopian/openbases/internal/registry"
)

func main() {
	var (
		in     = flag.String("f", "-", "desired-state document, or - for stdin")
		dryRun = flag.Bool("dry-run", false, "validate and print, writing nothing")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	doc, err := read(*in)
	if err != nil {
		log.Error("the registry document could not be read", "error", err)
		os.Exit(2)
	}
	if err := doc.Validate(); err != nil {
		// Exit 2, not 1: this is a bad document, which is a different problem
		// from a database that would not take a good one, and a deployment
		// should be able to tell them apart.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	// Reported before anything is written, because a shared cell is the reason
	// that cell's spend will not attribute, and finding that out from a cost
	// report weeks later is how it becomes a mystery.
	for _, cell := range sortedKeys(doc.SharedCells()) {
		log.Warn("this cell hosts more than one project, so its spend cannot be attributed to any of them",
			"cell", cell, "projects", strings.Join(doc.SharedCells()[cell], ", "))
	}

	if *dryRun {
		fmt.Printf("node %s (%s), %d cell(s), %d project attachment(s): document is valid, nothing written\n",
			doc.Node.Hostname, doc.Node.Environment, len(doc.Cells), len(doc.Projects))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db, err := open(ctx)
	if err != nil {
		log.Error("the database is unreachable, so nothing can be registered", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := apply(ctx, db, doc); err != nil {
		log.Error("registration failed", "error", err)
		os.Exit(1)
	}
}

func apply(ctx context.Context, db *sql.DB, doc registry.Document) error {
	// One transaction. A node registered without its cells leaves the registry
	// describing a machine that appears to host nothing, which reads exactly
	// like the bug this program exists to fix.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var changed bool
	if err := tx.QueryRowContext(ctx,
		`SELECT system_register_execution_node($1, $2)`,
		doc.Node.Hostname, doc.Node.Environment).Scan(&changed); err != nil {
		return fmt.Errorf("registering node %s: %w", doc.Node.Hostname, err)
	}
	fmt.Printf("node      %-36s %-14s (%s)\n", doc.Node.Hostname, doc.Node.Environment, state(changed))

	for _, c := range doc.Cells {
		if err := tx.QueryRowContext(ctx,
			`SELECT system_register_execution_cell($1,$2,$3,$4,$5,$6,$7,$8)`,
			doc.Node.Hostname, c.Slug, c.SystemUsername, c.TrustDomain,
			nullableInt(c.MaxConcurrentAgents), nullableInt(c.CPUQuotaPercent), nullableInt(c.MemoryLimitMB),
			c.Shared,
		).Scan(&changed); err != nil {
			return fmt.Errorf("registering cell %s: %w", c.Slug, err)
		}
		shared := ""
		if c.Shared {
			// Worth printing: it is what decides where a project with no cell
			// named goes, so a deploy that changes it should say so.
			shared = " shared"
		}
		fmt.Printf("cell      %-36s user=%s trust=%s cpu=%d%% mem=%dMB agents=%d%s (%s)\n",
			c.Slug, c.SystemUsername, c.TrustDomain, c.CPUQuotaPercent, c.MemoryLimitMB,
			c.MaxConcurrentAgents, shared, state(changed))
	}

	// Graphs before projects, because a project graph names a project and the
	// function refuses one that is not registered -- so if a document ever
	// declares both, the clearer failure is the graph complaining about a
	// missing project rather than a project silently having no graph.
	for _, g := range doc.Graphs {
		// Returns a phrase rather than a boolean: the graph functions report
		// registered, updated or unchanged, and Ansible greps for "(updated)".
		var result string
		if err := tx.QueryRowContext(ctx,
			`SELECT system_register_beads_graph($1,$2,$3,$4,$5)`,
			g.Name, g.Path, doc.Node.Hostname, g.Scope, nullableStr(g.Project),
		).Scan(&result); err != nil {
			return fmt.Errorf("registering graph %s: %w", g.Name, err)
		}
		project := "-"
		if g.Project != "" {
			project = g.Project
		}
		fmt.Printf("graph     %-36s scope=%s project=%s path=%s %s\n",
			g.Name, g.Scope, project, g.Path, resultState(result))
	}

	for _, p := range doc.Projects {
		if err := tx.QueryRowContext(ctx,
			`SELECT system_attach_project_to_cell($1,$2,$3)`,
			p.Slug, p.Cell, nullableStr(p.Visibility)).Scan(&changed); err != nil {
			return fmt.Errorf("attaching project %s to cell %s: %w", p.Slug, p.Cell, err)
		}
		vis := ""
		if p.Visibility != "" {
			vis = " visibility=" + p.Visibility
		}
		fmt.Printf("project   %-36s cell=%s%s (%s)\n", p.Slug, p.Cell, vis, state(changed))
	}

	return tx.Commit()
}

// resultState turns the function's phrase into the word Ansible greps for, so
// the graph rows read the same as every other row rather than exposing a second
// vocabulary in one output.
func resultState(result string) string {
	switch {
	case strings.HasSuffix(result, "(registered)"), strings.HasSuffix(result, "(updated)"):
		return "(updated)"
	default:
		return "(unchanged)"
	}
}

// state is the word Ansible greps for. A deployment must be able to report that
// it changed nothing (WP-B2), which means every line has to say which it was.
func state(changed bool) string {
	if changed {
		return "updated"
	}
	return "unchanged"
}

func read(path string) (registry.Document, error) {
	var doc registry.Document
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return doc, err
		}
		defer f.Close()
		r = f
	}
	dec := json.NewDecoder(io.LimitReader(r, 1<<20))
	// An unknown field is almost always a misspelled one, and silently ignoring
	// it means a limit or a trust domain that was set and had no effect.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return doc, err
	}
	return doc, nil
}

// nullableInt keeps an unset limit NULL rather than zero. Zero CPU is a real and
// very different instruction from "no limit recorded".
func nullableInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullableStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func sortedKeys(m map[string][]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func open(ctx context.Context) (*sql.DB, error) {
	dsn := config.DatabaseURL()
	if dsn == "" {
		return nil, errors.New("no database URL is configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
