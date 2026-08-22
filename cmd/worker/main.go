// Command worker runs background jobs: reconciliation, summaries, extraction,
// context packs, evaluation, notifications, and backups (plan section 6.1).
//
// Jobs are claimed from a PostgreSQL-backed queue with FOR UPDATE SKIP LOCKED.
// No Redis is introduced in version 1.
package main

import (
	"context"
	"database/sql"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/workgraph/internal/config"
	"github.com/datopian/workgraph/internal/reconcile"
	"github.com/datopian/workgraph/internal/version"
)

func main() {
	interval := flag.Duration("interval", 5*time.Second, "how often to look for unprocessed deliveries")
	batch := flag.Int("batch", 200, "most deliveries to project in one pass")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	log.Info("starting worker", "build", version.String(), "interval", interval.String())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := config.DatabaseURL()
	if dsn == "" {
		log.Error("no database URL is configured; the worker has nothing to do")
		os.Exit(1)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Error("the database driver refused the connection string", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	// Small: this polls one query on a short tick and projects in one goroutine.
	db.SetMaxOpenConns(4)

	if err := run(ctx, log, db, *interval, *batch); err != nil {
		log.Error("worker failed", "error", err)
		os.Exit(1)
	}
	log.Info("stopped cleanly")
}

// run is the job loop.
//
// Its first real handler projects inbound GitHub deliveries. Until it existed
// nothing consumed github_deliveries except the reconciliation timer every
// fifteen minutes, and the numbers said exactly that: measured over 3287
// deliveries on staging, event freshness was 459s at p50 and 940s at p95 against
// a documented target of under 60 seconds.
//
// Reconciliation was never meant to be the primary path — its own timer says it
// is "what catches a delivery that was never sent" — and it had become the only
// one, because the loop below logged "no handlers registered yet" instead of
// doing the work.
//
// Polling rather than LISTEN/NOTIFY. A five-second poll of one indexed query is
// cheap, and it recovers on its own from a delivery that arrived while the
// worker was down. NOTIFY would be lower latency and would silently lose
// anything published while nobody was listening, which is the failure this
// system keeps finding in other guises.
func run(ctx context.Context, log *slog.Logger, db *sql.DB, interval time.Duration, batch int) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			res, ran, err := reconcile.ReplayIfIdle(ctx, db, log, batch)
			switch {
			case err != nil:
				// Logged and not returned: a transient database error must not
				// stop the loop, or one blip leaves deliveries unprocessed until
				// somebody notices the unit is gone.
				log.Error("projecting deliveries failed", "error", err)
			case !ran:
				// Reconciliation holds the lock. Nothing to say; it will run on
				// the next tick.
			case res.Replayed > 0 || res.ReplayFailed > 0 || res.StillUnresolved > 0:
				// Only when something happened. At this interval, logging every
				// idle pass would bury the passes that matter.
				log.Info("projected deliveries", res.LogArgs()...)
			}
		}
	}
}
