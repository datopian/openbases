// Command loadtest exercises the control plane concurrently and checks that
// row-level security holds while it does (WP-I3).
//
// The interesting risk is not throughput, it is IDENTITY LEAKING BETWEEN
// REQUESTS. authz.WithUser sets workgraph.user_id with is_local = true, so it is
// scoped to the transaction and released when the transaction ends. That is the
// correct choice, and it is exactly the kind of correct choice that is worth
// proving rather than asserting: a connection pool hands the same physical
// connection to different users in succession, and if the setting were session
// scoped instead, user B would inherit user A's identity and read their
// projects. The bug would be invisible under serial testing and catastrophic
// under load.
//
// So the pool is deliberately SMALLER than the number of concurrent users. With
// 25 users and 5 connections, every connection is reused across identities many
// times per second, which is the condition the bug needs.
//
// Each worker's expected project set is computed once, serially, before the
// concurrent phase. Comparing against a precomputed expectation is what makes
// this a real test: if a worker asked "which projects am I a member of" during
// the concurrent phase and identity had leaked, the membership answer would leak
// in the same direction and agree with itself.
//
// Run ON the control node, against the load database:
//
//	loadtest -dsn "postgres://workgraph_app:PW@127.0.0.1:5432/workgraph_load?sslmode=disable" \
//	         -users 25 -conns 5 -seconds 60
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/workgraph/internal/authz"
)

type worker struct {
	userID   string
	email    string
	expected map[string]bool // project ids this user may see
}

func main() {
	var (
		dsn     = flag.String("dsn", os.Getenv("WG_LOAD_DSN"), "PostgreSQL connection string")
		users   = flag.Int("users", 25, "concurrent users (the WP-I3 target is 25 active agents)")
		conns   = flag.Int("conns", 5, "max pool connections; deliberately fewer than users")
		seconds = flag.Int("seconds", 60, "how long to run")
		streams = flag.Int("streams", 10, "concurrent read streams per the load target")
	)
	flag.Parse()

	if *dsn == "" {
		fail("no DSN; pass -dsn or set WG_LOAD_DSN")
	}

	db, err := sql.Open("pgx", *dsn)
	if err != nil {
		fail("opening the database: %v", err)
	}
	defer db.Close()

	// Fewer connections than workers, on purpose. See the package comment.
	db.SetMaxOpenConns(*conns)
	db.SetMaxIdleConns(*conns)

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		fail("cannot reach the database: %v", err)
	}

	ws, err := loadWorkers(ctx, db, *users)
	if err != nil {
		fail("preparing workers: %v", err)
	}
	fmt.Printf("prepared %d users; pool limited to %d connections\n", len(ws), *conns)
	for _, w := range ws[:min(3, len(ws))] {
		fmt.Printf("  %s expects %d project(s)\n", w.email, len(w.expected))
	}

	var (
		queries   atomic.Int64
		violation atomic.Int64
		errs      atomic.Int64
		latMu     sync.Mutex
		latencies []time.Duration
	)

	deadline := time.Now().Add(time.Duration(*seconds) * time.Second)
	var wg sync.WaitGroup

	// `streams` readers in total. Each picks the next user round-robin, so a
	// single pooled connection serves many identities in succession — which is
	// the condition an identity leak needs — without inventing contention that
	// the load target does not describe.
	var next atomic.Int64
	for s := 0; s < *streams; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				w := ws[int(next.Add(1))%len(ws)]
				start := time.Now()
				seen, err := visibleProjects(ctx, db, w.userID)
				took := time.Since(start)
				if err != nil {
					errs.Add(1)
					continue
				}
				queries.Add(1)
				latMu.Lock()
				latencies = append(latencies, took)
				latMu.Unlock()

				// The assertion. Any project outside the precomputed set is
				// identity bleeding across a pooled connection.
				for id := range seen {
					if !w.expected[id] {
						violation.Add(1)
						fmt.Printf("LEAK: %s saw project %s, which is not in its %d expected\n",
							w.email, id, len(w.expected))
					}
				}
				// The reverse also matters: seeing FEWER projects than expected
				// means RLS denied something it should have allowed, which is a
				// different bug with the same cause.
				if len(seen) != len(w.expected) {
					violation.Add(1)
					fmt.Printf("MISMATCH: %s saw %d project(s), expected %d\n",
						w.email, len(seen), len(w.expected))
				}
			}
		}()
	}
	wg.Wait()

	latMu.Lock()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	latMu.Unlock()

	n := int64(len(latencies))
	fmt.Printf("\n%d queries in %ds: %d streams cycling %d users over %d connections\n",
		queries.Load(), *seconds, *streams, len(ws), *conns)
	if n > 0 {
		fmt.Printf("  throughput %.0f queries/sec\n", float64(n)/float64(*seconds))
		fmt.Printf("  p50 %v   p95 %v   p99 %v   max %v\n",
			latencies[n*50/100], latencies[n*95/100], latencies[min64(n*99/100, n-1)], latencies[n-1])
	}
	fmt.Printf("  errors %d\n", errs.Load())
	fmt.Printf("  ISOLATION VIOLATIONS %d\n", violation.Load())

	if violation.Load() > 0 {
		fail("row-level security did not hold under concurrency")
	}
	if queries.Load() == 0 {
		fail("no queries completed; this run proves nothing")
	}
	fmt.Println("\nrow-level security held under concurrency")
}

// loadWorkers picks users and computes each one's expected project set, once,
// serially. Serial on purpose: this is the baseline the concurrent phase is
// measured against, so it must not itself be subject to the bug being hunted.
func loadWorkers(ctx context.Context, db *sql.DB, want int) ([]worker, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id::text, primary_email FROM users
		  WHERE primary_email LIKE 'load-user-%'
		  ORDER BY primary_email LIMIT $1`, want)
	if err != nil {
		return nil, err
	}
	var ws []worker
	for rows.Next() {
		var w worker
		if err := rows.Scan(&w.userID, &w.email); err != nil {
			rows.Close()
			return nil, err
		}
		ws = append(ws, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ws) == 0 {
		return nil, fmt.Errorf("no load-user-%% rows; run scripts/load_env.sh seed first")
	}

	for i := range ws {
		seen, err := visibleProjects(ctx, db, ws[i].userID)
		if err != nil {
			return nil, fmt.Errorf("baseline for %s: %w", ws[i].email, err)
		}
		if len(seen) == 0 {
			return nil, fmt.Errorf("%s can see no projects; the fixture would make this test vacuous",
				ws[i].email)
		}
		ws[i].expected = seen
	}
	return ws, nil
}

// visibleProjects asks, as this user and through the real identity path, which
// projects their work items belong to.
func visibleProjects(ctx context.Context, db *sql.DB, userID string) (map[string]bool, error) {
	out := map[string]bool{}
	err := authz.WithUser(ctx, db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT DISTINCT project_id::text FROM work_refs
			  WHERE project_id IS NOT NULL AND status = 'open'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out[id] = true
		}
		return rows.Err()
	})
	return out, err
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "loadtest: "+format+"\n", args...)
	os.Exit(1)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
