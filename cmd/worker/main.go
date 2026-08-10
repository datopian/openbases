// Command worker runs background jobs: reconciliation, summaries, extraction,
// context packs, evaluation, notifications, and backups (plan section 6.1).
//
// Jobs are claimed from a PostgreSQL-backed queue with FOR UPDATE SKIP LOCKED.
// No Redis is introduced in version 1.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/datopian/workgraph/internal/version"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	log.Info("starting worker", "build", version.String())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, log); err != nil {
		log.Error("worker failed", "error", err)
		os.Exit(1)
	}
	log.Info("stopped cleanly")
}

// run is the job loop. Job types and the PostgreSQL claim query land in WP-C1
// and WP-D3; the loop and its shutdown semantics are established here so that
// later work adds handlers rather than lifecycle code.
func run(ctx context.Context, log *slog.Logger) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Claiming and dispatching jobs is implemented by WP-C1.
			log.Debug("job poll tick (no handlers registered yet)")
		}
	}
}
