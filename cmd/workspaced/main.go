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
	"github.com/datopian/workgraph/internal/inference"
	"github.com/datopian/workgraph/internal/ingest"
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
		phase = flag.String("phase", "all",
			"all, reconcile (subscriptions only) or ingest (registered sources only)")
		ingestLimit = flag.Int("ingest-limit", 50, "most deliveries to ingest in one pass")
		timeout     = flag.Duration("timeout", 15*time.Minute, "overall deadline")
		verbose     = flag.Bool("v", false, "log the sources left alone too")
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

	// The event types come from the embedded sources.json, which is the copy
	// that was established against the live API one type at a time. Retyping
	// them into Go is how the first deployment came to ask for
	// permission.v3.updated — which does not exist, so Google refused the whole
	// create and three of four sources silently never subscribed.
	verified, err := workspace.VerifiedWants()
	if err != nil {
		fail(log, "reading the verified source definitions", err)
	}

	if *summary {
		if err := printSummary(ctx, store, *since); err != nil {
			fail(log, "summarising deliveries", err)
		}
		return
	}

	var meetClient *workspace.Meet

	r := &workspace.Reconciler{
		Store:  store,
		Topic:  *topic,
		Wants:  verified,
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
		meetClient = &workspace.Meet{Token: tokens.Token}
		log.Info("reconciling", "project", sa.ProjectID, "subject", *subject, "topic", *topic)
	}

	failedSomething := false

	if *phase == "all" || *phase == "reconcile" {
		rep, err := r.Run(ctx)
		if err != nil {
			fail(log, "reconciliation could not run", err)
		}
		log.Info("reconciliation finished",
			"decisions", len(rep.Outcomes), "changed", rep.Changed, "failed", rep.Failed)
		if rep.Failed > 0 {
			log.Error("some sources could not be reconciled", "error", rep.Err())
			failedSomething = true
		}
	}

	// Ingestion runs AFTER reconciliation and only with a credential, in the
	// same pass rather than on a timer of its own. One unit, because the two
	// need exactly the same things -- the delegation, the database and the
	// allow-list -- and a second timer would be a second thing to notice had
	// stopped.
	//
	// The cost is latency: a transcript is ready within minutes of a meeting
	// and waits up to an hour to be registered. Acceptable while nothing acts
	// on it automatically; worth revisiting when extraction lands (WP-H3),
	// because "the platform knew an hour ago" is a different product.
	if (*phase == "all" || *phase == "ingest") && !*dryRun && r.Events != nil {
		in := &ingest.Ingestor{DB: db, Meet: meetClient, Log: log,
			Inference: inferenceClient(log)}
		res, err := in.Run(ctx, *ingestLimit)
		if err != nil {
			fail(log, "ingestion could not run", err)
		}
		log.Info("ingestion finished",
			"considered", res.Considered, "registered", res.Registered,
			"skipped", res.Skipped, "failed", res.Failed)
		if res.Failed > 0 {
			failedSomething = true
		}
	}

	if failedSomething {
		// Non-zero so the systemd unit records a failure and the monitor sees
		// it. A pass that could not renew a subscription, or could not register
		// a transcript, has not succeeded even though it did not crash.
		os.Exit(1)
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

// inferenceClient builds the extraction client, or nil.
//
// Nil is a supported mode: registering what a source WAS must not depend on a
// model being reachable, so a missing gateway token degrades to ingestion
// without extraction rather than failing the pass.
func inferenceClient(log *slog.Logger) *inference.Client {
	base := os.Getenv("WG_AI_GATEWAY_BASE_URL")
	// The same accessor every other caller uses, so the token arrives the same
	// way here as it does for an agent run: a systemd credential first, the
	// environment second.
	token := config.AIGatewayToken()
	if base == "" || token == "" {
		log.Warn("no gateway credentials; registering sources without extracting candidates")
		return nil
	}
	return inference.New(base, token)
}

func fail(log *slog.Logger, msg string, err error) {
	if err != nil {
		log.Error(msg, "error", err)
	} else {
		log.Error(msg)
	}
	os.Exit(1)
}
