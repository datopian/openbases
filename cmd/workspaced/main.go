// Command workspaced keeps Google Workspace event subscriptions alive.
//
// Run on a timer. Every pass is idempotent: it reads what we believe, asks what
// Google holds, and makes the smallest change that closes the gap. Running it
// twice in a row changes nothing the second time, which is the property that
// makes "renewed and reconciled without duplicate effects" hold.
//
// Google expires a Workspace Events subscription within days — hours for some
// targets — so this timer is not maintenance, it is the thing that keeps
// discovery working. If it stops, sources go quiet one at a time with nothing
// to notice.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/workgraph/internal/config"
	"github.com/datopian/workgraph/internal/workspace"
)

func main() {
	var (
		dryRun = flag.Bool("dry-run", false,
			"decide and print, but call nothing and write nothing")
		subject = flag.String("subject", os.Getenv("WG_GOOGLE_SUBJECT"),
			"the Workspace user to impersonate through domain-wide delegation")
		topic = flag.String("topic", os.Getenv("WG_GOOGLE_PUBSUB_TOPIC"),
			"the Pub/Sub topic subscriptions notify")
		keyPath = flag.String("key", os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
			"path to the service-account key")
		summary = flag.Bool("summary", false,
			"print what has been delivered per source and exit")
		since = flag.Duration("since", 7*24*time.Hour,
			"how far back -summary looks")
		timeout = flag.Duration("timeout", 5*time.Minute, "overall deadline")
		verbose = flag.Bool("v", false, "log the sources left alone too")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Shares the control API's assembly so a password held as a systemd
	// credential works here too.
	dsn := config.DatabaseURL()
	if dsn == "" {
		fail(log, "no database connection string; set WG_DATABASE_URL, or "+
			"WG_DATABASE_URL_TEMPLATE with a db_app_password credential", nil)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fail(log, "opening the database", err)
	}
	defer db.Close()

	store := &workspace.DB{Conn: db}

	if *summary {
		if err := printSummary(ctx, store, *since); err != nil {
			fail(log, "summarising deliveries", err)
		}
		return
	}

	r := &workspace.Reconciler{
		Store:  store,
		Topic:  *topic,
		Wants:  wants(),
		Policy: workspace.DefaultPolicy,
		Log:    log,
		DryRun: *dryRun,
	}

	// The credential is loaded only when we intend to call Google. A dry run
	// must work on a laptop with no key, or nobody will ever use it to check
	// what a pass would do before letting the timer do it.
	if !*dryRun {
		if *keyPath == "" {
			fail(log, "no service-account key; set GOOGLE_APPLICATION_CREDENTIALS "+
				"(scripts/with_secrets.sh materialises it) or pass -dry-run", nil)
		}
		sa, err := workspace.LoadServiceAccount(*keyPath)
		if err != nil {
			fail(log, "loading the service-account key", err)
		}
		if *subject == "" {
			fail(log, "no delegation subject; set WG_GOOGLE_SUBJECT to a Workspace "+
				"user, since a service account cannot hold Drive or Meet resources itself", nil)
		}
		tokens := &workspace.DelegatedTokens{
			Account: sa,
			Subject: *subject,
			Scopes:  []string{workspace.ScopeDriveReadonly, workspace.ScopeMeetReadonly},
		}
		r.Events = &workspace.Events{Token: tokens.Token}
		log.Info("reconciling", "project", sa.ProjectID, "subject", *subject, "topic", *topic)
	}

	rep, err := r.Run(ctx)
	if err != nil {
		fail(log, "reconciliation could not run", err)
	}
	log.Info("pass finished",
		"decisions", len(rep.Outcomes), "changed", rep.Changed, "failed", rep.Failed)
	if rep.Failed > 0 {
		// Non-zero so the systemd unit records a failure and the monitor sees
		// it. A pass that could not renew a subscription has not succeeded,
		// even though it did not crash.
		log.Error("some sources could not be reconciled", "error", rep.Err())
		os.Exit(1)
	}
}

// wants is the event types we subscribe to, per kind.
//
// In code, not configuration. Every one of these was established by
// validateOnly against the live API, and the set is not free to vary: Drive
// rejects `file.v3.updated` (there is no such type) and rejects
// `file.v3.content` for a shared-drive target, while Meet rejects
// `transcript.v2.file`. A wrong entry does not degrade gracefully — the whole
// create call is refused, so the source silently never subscribes.
func wants() workspace.Wants {
	return workspace.Wants{
		workspace.KindDrive: {
			"google.workspace.drive.file.v3.created",
			"google.workspace.drive.file.v3.deleted",
			"google.workspace.drive.file.v3.moved",
			"google.workspace.drive.file.v3.renamed",
			"google.workspace.drive.file.v3.trashed",
			"google.workspace.drive.permission.v3.created",
			"google.workspace.drive.permission.v3.deleted",
			"google.workspace.drive.permission.v3.updated",
		},
		workspace.KindMeet: {
			// conference.v2.ended is what tells us a meeting happened at all;
			// transcript.v2.ended is what tells us there is something to read.
			// Both, because the meeting is sometimes held with transcription
			// off and sometimes skipped entirely, and "no transcript" is a fact
			// worth knowing rather than silence.
			"google.workspace.meet.conference.v2.ended",
			"google.workspace.meet.transcript.v2.ended",
		},
	}
}

func printSummary(ctx context.Context, store *workspace.DB, since time.Duration) error {
	rows, err := store.Summarise(ctx, time.Now().Add(-since))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Printf("no events received in the last %s\n", since)
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

func fail(log *slog.Logger, msg string, err error) {
	if err != nil {
		log.Error(msg, "error", err)
	} else {
		log.Error(msg)
	}
	os.Exit(1)
}
