// Command wg-work drives the work queue from the control node.
//
//	wg-work sync-hq                 project the company graph into the UI
//	wg-work plan "a brief"          queue a planning job
//	wg-work dispatch wg-abc         queue one bead
//	wg-work list                    what work exists, and what it cost
//	wg-work queue                   what is queued, running or finished
//
// The same queue the interface uses and the execution node claims from, reached
// directly rather than over HTTP because this runs where the database is. It
// exists for two reasons: a demo or an incident should not depend on a browser,
// and sync-hq can fill the interface from the company graph on this node without
// any execution node participating at all.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/workgraph/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db, err := open(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()

	args := os.Args[2:]
	switch os.Args[1] {
	case "plan":
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, `usage: wg-work plan "a brief" [cell]`)
			os.Exit(2)
		}
		cell := cellOr(args, 1)
		var id string
		if err := db.QueryRowContext(ctx,
			`SELECT system_enqueue_work('plan', $1, 'sandbox', NULL, $2, NULL)`,
			cell, args[0]).Scan(&id); err != nil {
			fail(err)
		}
		fmt.Printf("queued plan %s on %s\n", id, cell)

	case "dispatch":
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "usage: wg-work dispatch <bead> [cell]")
			os.Exit(2)
		}
		cell := cellOr(args, 1)
		var id string
		if err := db.QueryRowContext(ctx,
			`SELECT system_enqueue_work('work', $1, 'sandbox', $2, NULL, NULL)`,
			cell, args[0]).Scan(&id); err != nil {
			fail(err)
		}
		fmt.Printf("queued %s for %s on %s\n", id, args[0], cell)

	case "list":
		listWork(ctx, db)

	case "queue":
		listQueue(ctx, db)

	case "sync-hq":
		syncHQ(ctx, db)

	default:
		usage()
		os.Exit(2)
	}
}

// syncHQ projects the company graph on THIS node into work_refs.
//
// The interface reads work_refs, which nothing filled until the execution node's
// dispatcher started pushing beads up. That path needs the node to be reachable
// and configured; this one needs neither, because the company graph and the
// database are both on this machine. It is how the interface shows real work
// without an execution node participating at all.
func syncHQ(ctx context.Context, db *sql.DB) {
	graph := getenv("WG_HQ_GRAPH", "/srv/graphs/company-hq")
	cell := getenv("WG_HQ_CELL", "oss")

	cmd := exec.CommandContext(ctx, "bd", "list", "--all", "--json")
	cmd.Dir = graph
	cmd.Env = append(os.Environ(), "HOME="+graph)
	out, err := cmd.Output()
	if err != nil {
		fail(fmt.Errorf("bd list in %s: %w", graph, err))
	}

	var raw []map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		var wrapper struct {
			Issues []map[string]any `json:"issues"`
		}
		if err2 := json.Unmarshal(out, &wrapper); err2 != nil {
			fail(fmt.Errorf("bd list returned neither a list nor {issues}: %w", err))
		}
		raw = wrapper.Issues
	}

	n := 0
	for _, r := range raw {
		id, _ := r["id"].(string)
		if strings.TrimSpace(id) == "" {
			continue
		}
		title, _ := r["title"].(string)
		kind, _ := r["issue_type"].(string)
		if kind == "" {
			kind, _ = r["type"].(string)
		}
		status, _ := r["status"].(string)
		var ok bool
		if err := db.QueryRowContext(ctx,
			`SELECT system_project_bead($1,$2,$3,$4,$5)`, cell, id, title, kind, status).
			Scan(&ok); err != nil {
			fail(err)
		}
		n++
	}
	fmt.Printf("projected %d bead(s) from %s onto cell %s\n", n, graph, cell)
}

func listWork(ctx context.Context, db *sql.DB) {
	rows, err := db.QueryContext(ctx,
		`SELECT bead, coalesce(title,''), coalesce(status,''), coalesce(queue_state,''),
		        spent_cents::text, requests
		   FROM system_work_overview(NULL) LIMIT 60`)
	if err != nil {
		fail(err)
	}
	defer rows.Close()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BEAD\tSTATUS\tQUEUE\tCENTS\tREQS\tTITLE")
	n := 0
	for rows.Next() {
		var bead, title, status, queue, cents string
		var reqs int64
		if err := rows.Scan(&bead, &title, &status, &queue, &cents, &reqs); err != nil {
			fail(err)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", bead, status, dash(queue), trimCents(cents), reqs, truncate(title, 52))
		n++
	}
	w.Flush()
	if n == 0 {
		fmt.Println("\nno work is projected yet: run: wg-work sync-hq  — or start the dispatcher on an execution node")
	}
}

func listQueue(ctx context.Context, db *sql.DB) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, kind, cell, coalesce(bead,''), status, created_at,
		        coalesce(left(coalesce(result, brief, ''), 60), '')
		   FROM system_queue_overview(NULL) LIMIT 40`)
	if err != nil {
		fail(err)
	}
	defer rows.Close()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "JOB\tKIND\tCELL\tBEAD\tSTATUS\tAGE\tDETAIL")
	n := 0
	for rows.Next() {
		var id, kind, cell, bead, status, detail string
		var created time.Time
		if err := rows.Scan(&id, &kind, &cell, &bead, &status, &created, &detail); err != nil {
			fail(err)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			id[:8], kind, cell, dash(bead), status,
			time.Since(created).Round(time.Second), strings.ReplaceAll(truncate(detail, 56), "\n", " "))
		n++
	}
	w.Flush()
	if n == 0 {
		fmt.Println("\nthe queue is empty")
	}
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

func cellOr(args []string, i int) string {
	if len(args) > i && strings.TrimSpace(args[i]) != "" {
		return args[i]
	}
	return "oss"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// trimCents renders numeric(16,8) for a person rather than for a column.
func trimCents(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, strings.TrimLeft(`
wg-work — drive the work queue from the control node

  wg-work sync-hq                 project the company graph into the interface
  wg-work plan "a brief" [cell]   queue a planning job: brief in, beads out
  wg-work dispatch <bead> [cell]  queue one bead for an agent
  wg-work list                    what work exists, and what it cost
  wg-work queue                   what is queued, running or finished

Planning and dispatch are QUEUED, not run. The control plane cannot reach an
execution node — nodes have no inbound port — so the node claims the job on its
next pass. Watch it move with: wg-work queue
`, "\n"))
}
