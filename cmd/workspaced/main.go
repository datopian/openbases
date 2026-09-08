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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/openbases/internal/beads"
	"github.com/datopian/openbases/internal/config"
	"github.com/datopian/openbases/internal/githubapp"
	"github.com/datopian/openbases/internal/inference"
	"github.com/datopian/openbases/internal/ingest"
	"github.com/datopian/openbases/internal/publish"
	"github.com/datopian/openbases/internal/workspace"
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
			"all, reconcile (subscriptions only), ingest (registered sources only) "+
				"or publish (accepted candidates only)")
		ingestLimit  = flag.Int("ingest-limit", 50, "most deliveries to ingest in one pass")
		publishLimit = flag.Int("publish-limit", 50, "most accepted candidates to publish in one pass")
		beadsBinary  = flag.String("bd", envOr("WG_BEADS_BINARY", "/usr/local/bin/bd"),
			"the pinned bd binary that creates beads for accepted candidates")
		node = flag.String("node", os.Getenv("WG_NODE_NAME"),
			"this host's name as the registry records it; graphs on another host are left alone")
		knowledgeRepo = flag.String("knowledge-repo", os.Getenv("WG_KNOWLEDGE_REPO"),
			"owner/name of the repository accepted records are proposed in; empty publishes no Markdown")
		knowledgeBase = flag.String("knowledge-base", envOr("WG_KNOWLEDGE_BASE", "main"),
			"the branch pull requests target")
		recordLimit = flag.Int("record-limit", 20, "most accepted records to propose in one pass")
		timeout     = flag.Duration("timeout", 15*time.Minute, "overall deadline")
		verbose     = flag.Bool("v", false, "log the sources left alone too")
		// A deployment declares whether it ingests Workspace at all. Drive and
		// Meet need a GCP project, domain-wide delegation and Pub/Sub, and
		// nothing else in the system depends on them.
		// Named ingestOn, not ingest: internal/ingest is imported here and a
		// variable called ingest shadows the package. The same shadowing cost a
		// build in cmd/migrate an hour earlier.
		ingestOn = flag.Bool("ingest", os.Getenv("WG_WORKSPACE_INGEST") == "true",
			"this deployment ingests Google Drive and Meet")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	// Nothing to keep alive if this deployment does not ingest Workspace.
	//
	// Exits 0 rather than failing: on a deployment that never set Drive and
	// Meet up, the reconciler refuses an empty source list by design -- it
	// cannot tell "never configured" from "configuration disappeared", and
	// treating the second as the first would delete every subscription. That
	// refusal is right for us and wrong for them, and the difference is not
	// observable, so the deployment declares which it is.
	//
	// Ansible does not install the timer when this is off, so reaching here
	// means a manual run or a leftover unit. Either way a clear line and a
	// zero exit beats a unit in failed state that nobody can clear.
	if !*ingestOn {
		log.Info("Google Workspace ingestion is off for this deployment; nothing to do",
			"hint", "set WG_WORKSPACE_INGEST=true, or -ingest, to enable it")
		return
	}

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

	// Publication runs last, and needs no Google credential: it reads accepted
	// candidates from the database and writes beads on this host's disk. In
	// the same pass for the reason ingestion is -- one unit to notice had
	// stopped -- but deliberately not gated on the credential, so a pass that
	// cannot reach Google still turns yesterday's accepted work into work.
	if *phase == "all" || *phase == "publish" {
		if *dryRun {
			log.Info("publication skipped; a dry run writes nothing")
		} else {
			pub := &publish.Publisher{
				DB:   db,
				Node: *node,
				Log:  log,
				Beads: publish.CLI{CLIClient: &beads.CLIClient{
					Binary: *beadsBinary,
					// HOME per graph, because Dolt keeps its config under
					// HOME and this node's service account has no home
					// directory -- beads_hq initialised each graph with HOME
					// pointing at the graph itself.
					HomeAtDatabasePath: true,
					// The fallback actor. Every candidate that has a
					// reviewer is attributed to them instead.
					Actor: "workgraph-publisher",
				}},
			}
			res, err := pub.Run(ctx, *publishLimit)
			if err != nil {
				fail(log, "publication could not run", err)
			}
			log.Info("publication finished",
				"published", res.Published, "adopted", res.Adopted,
				"blocked", res.Blocked, "elsewhere", res.Elsewhere, "failed", res.Failed)
			// Only a FAILURE fails the pass. A blocked candidate is logged
			// as an error and left queued, because it needs a human decision
			// -- a project assigned, or a graph registered -- and no number of
			// retries changes it. A timer that goes red every hour forever is
			// how people learn to ignore the hour it matters.
			if res.Failed > 0 {
				failedSomething = true
			}

			// The durable half: an accepted record proposed as Markdown, and
			// an accepted decision given its bead. Runs in the same pass
			// because it needs the same database and the same graphs; skipped
			// with a word rather than silently when the repository or the
			// GitHub App is not configured, because "no Markdown appeared" is
			// otherwise indistinguishable from "nothing was accepted".
			recPub := &publish.RecordPublisher{
				DB:    db,
				Beads: pub.Beads,
				Owner: repoOwner(*knowledgeRepo),
				Repo:  repoName(*knowledgeRepo),
				Base:  *knowledgeBase,
				Node:  *node,
				Log:   log,
			}
			// Without a repository the pass still creates decision beads: a
			// bead needs nothing from Git. Said out loud, because "no pull
			// request appeared" is otherwise indistinguishable from "nothing
			// was accepted".
			if *knowledgeRepo == "" {
				log.Info("no Markdown proposals; set WG_KNOWLEDGE_REPO to the repository " +
					"records live in. Decision beads are still published")
			} else if gh, err := githubClient(); err != nil {
				log.Error("no Markdown proposals; the GitHub App is not configured", "error", err)
				failedSomething = true
			} else {
				recPub.GitHub = gh
			}

			rres, err := recPub.Run(ctx, *recordLimit)
			if err != nil {
				fail(log, "record publication could not run", err)
			}
			log.Info("record publication finished",
				"proposed", rres.Proposed, "decision_beads", rres.Beads,
				"linked", rres.Linked, "blocked", rres.Blocked, "failed", rres.Failed)
			if rres.Failed > 0 {
				failedSomething = true
			}
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

// envOr is the default-with-an-override the other flags spell out inline.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// githubClient assembles the App from the same environment the other binaries
// read it from.
func githubClient() (*githubapp.Client, error) {
	appID := os.Getenv("WG_GITHUB_APP_ID")
	installation := os.Getenv("WG_GITHUB_INSTALLATION_ID")
	keyPath := os.Getenv("WG_GITHUB_PRIVATE_KEY_PATH")
	if appID == "" || installation == "" || keyPath == "" {
		return nil, errors.New("set WG_GITHUB_APP_ID, WG_GITHUB_INSTALLATION_ID and WG_GITHUB_PRIVATE_KEY_PATH")
	}
	key, err := githubapp.LoadPrivateKey(keyPath)
	if err != nil {
		return nil, err
	}
	return &githubapp.Client{AppID: appID, InstallationID: installation, PrivateKeyPEM: key}, nil
}

// repoOwner and repoName split an owner/name pair, and return nothing for
// anything that is not one -- a half-configured repository must not become a
// request against a guess.
func repoOwner(full string) string {
	owner, name, ok := strings.Cut(full, "/")
	if !ok || owner == "" || name == "" {
		return ""
	}
	return owner
}

func repoName(full string) string {
	owner, name, ok := strings.Cut(full, "/")
	if !ok || owner == "" || name == "" {
		return ""
	}
	return name
}
