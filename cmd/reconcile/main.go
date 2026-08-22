// Command reconcile recovers projection state that events alone cannot.
//
// Run on a timer. It is safe to run concurrently with the API and safe to run
// twice: every effect is a keyed upsert guarded on updated_at, so applying the
// same fact again changes nothing.
package main

import (
	"context"
	"database/sql"
	"flag"
	"log/slog"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/workgraph/internal/config"
	"github.com/datopian/workgraph/internal/githubapp"
	"github.com/datopian/workgraph/internal/reconcile"
)

func main() {
	var (
		replayOnly = flag.Bool("replay-only", false,
			"replay stored deliveries but do not call the GitHub API")
		limit   = flag.Int("limit", 500, "maximum receipts to replay in one pass")
		timeout = flag.Duration("timeout", 10*time.Minute, "overall deadline")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Shares the control API's assembly, so a password held as a systemd
	// credential works here too. Reading WG_DATABASE_URL directly is what
	// broke this when the password moved out of the environment.
	dsn := config.DatabaseURL()
	if dsn == "" {
		log.Error("no database connection string; set WG_DATABASE_URL, or WG_DATABASE_URL_TEMPLATE with a db_app_password credential")
		os.Exit(1)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Error("opening the database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// ReplayIfIdle, because the worker now replays every few seconds and the two
	// would otherwise read the same pending receipts and project them twice.
	// Skipping when the worker holds the lock is correct: it is already doing
	// this work, and reconciliation's own job — asking GitHub for state that
	// never arrived — happens below regardless.
	replay, ran, err := reconcile.ReplayIfIdle(ctx, db, log, *limit)
	if err == nil && !ran {
		log.Info("replay skipped; the worker is already projecting deliveries")
	}
	if err != nil {
		log.Error("replay", "error", err)
		os.Exit(1)
	}
	log.Info("replay complete", replay.LogArgs()...)

	total := replay

	// Resync needs the App credentials. Without them the replay half still
	// runs, because recovering what we already received should not depend on
	// being able to reach GitHub.
	if !*replayOnly {
		gh, err := clientFromEnv()
		if err != nil {
			log.Warn("skipping resync", "reason", err)
		} else {
			resync, err := reconcile.Resync(ctx, db, gh, log)
			if err != nil {
				log.Error("resync", "error", err)
				os.Exit(1)
			}
			log.Info("resync complete", resync.LogArgs()...)
			total.Resynced = resync.Resynced
			total.ResyncFailed = resync.ResyncFailed
			total.NotInstalled = resync.NotInstalled
		}
	}

	log.Info("reconciliation finished", total.LogArgs()...)

	// A pass that could not project something is not a success. Exiting
	// non-zero makes the timer's failure visible in systemd rather than
	// requiring somebody to read the logs to discover it.
	//
	// NotInstalled is deliberately excluded. A repository in an organisation the
	// App was never installed on fails identically on every run, and a unit that
	// is permanently failed is one nobody looks at — which would hide the
	// failures this exit code exists to surface.
	if total.ReplayFailed > 0 || total.ResyncFailed > 0 {
		os.Exit(1)
	}
}

func clientFromEnv() (*githubapp.Client, error) {
	appID := os.Getenv("WG_GITHUB_APP_ID")
	installation := os.Getenv("WG_GITHUB_INSTALLATION_ID")
	keyPath := os.Getenv("WG_GITHUB_PRIVATE_KEY_PATH")
	if appID == "" || installation == "" || keyPath == "" {
		return nil, errNoCredentials
	}
	key, err := githubapp.LoadPrivateKey(keyPath)
	if err != nil {
		return nil, err
	}
	return &githubapp.Client{
		AppID:          appID,
		InstallationID: installation,
		PrivateKeyPEM:  key,
	}, nil
}

var errNoCredentials = errNoCreds("GitHub App credentials are not configured")

type errNoCreds string

func (e errNoCreds) Error() string { return string(e) }
