// Command control-api serves the Workgraph HTTP API and the web application.
//
// The origin is reached only through Cloudflare Tunnel; it never listens on a
// public interface (plan sections 6.1, 11.2).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/workgraph/internal/approvals"
	"github.com/datopian/workgraph/internal/attention"
	"github.com/datopian/workgraph/internal/authn"
	"github.com/datopian/workgraph/internal/budget"
	"github.com/datopian/workgraph/internal/chiefofstaff"
	"github.com/datopian/workgraph/internal/config"
	"github.com/datopian/workgraph/internal/domain"
	"github.com/datopian/workgraph/internal/githubapp"
	"github.com/datopian/workgraph/internal/httplog"
	"github.com/datopian/workgraph/internal/version"
	"github.com/datopian/workgraph/internal/webui"
	"github.com/datopian/workgraph/internal/witness"
	"github.com/datopian/workgraph/internal/work"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.LoadControlAPI()
	if err != nil {
		log.Error("configuration is invalid; refusing to start", "error", err)
		os.Exit(1)
	}

	log.Info("starting control-api",
		"env", cfg.Environment, "addr", cfg.ListenAddr, "build", version.String())

	// Where each secret came from, by name, never by value.
	//
	// The point is that migrating to systemd credentials is verifiable rather
	// than assumed. The application falls back to an environment variable when
	// no credential file is present, which is what keeps local development and a
	// half-migrated deployment working — and is also exactly what would let a
	// migration silently not happen. Logging the source turns "we moved the
	// secrets" into something you can grep for.
	log.Info("credential sources",
		"db_app_password", config.CredentialSource("db_app_password", "WG_DB_APP_PASSWORD"),
		"github_webhook_secret", config.CredentialSource("github_webhook_secret", "WG_GITHUB_WEBHOOK_SECRET"))

	// A rotation that was started and never finished leaves this set, and a
	// deployment quietly accepting a secret somebody believes was retired is
	// worse than the outage window it was introduced to avoid. Warned about on
	// every start so it is noticed rather than inherited.
	if cfg.GitHubWebhookSecretPrevious != "" {
		log.Warn("the previous GitHub webhook secret is still accepted; " +
			"remove WG_GITHUB_WEBHOOK_SECRET_PREVIOUS once GitHub is signing with the new one")
	}

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		log.Error("opening database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	// Outside local development the origin refuses to start without a working
	// authenticator. A build that cannot verify a token must not serve traffic
	// while looking healthy.
	var auth authn.Authenticator
	if cfg.Environment == config.EnvLocal {
		auth = &authn.StaticAuthenticator{}
		log.Warn("local environment: Access validation is disabled")
	} else {
		auth = &authn.AccessValidator{
			TeamDomain: cfg.AccessTeamDomain,
			Audience:   cfg.AccessAudience,
			// The cell application's audience, accepted only on the path it
			// fronts. Without the binding this would be a service credential
			// for the whole API.
			PathAudiences: map[string]string{
				"/v1/integrations/github/installation-token": cfg.CellAccessAudience,
				// The witness reports from the same execution nodes with the
				// same service token, but through its own Access application
				// and so its own audience: minting a git credential and
				// writing to inboxes are different powers.
				"/v1/agent-health": cfg.CellHealthAccessAudience,
				// And its own again for the budget check, which only reads.
				// Three narrow applications sharing one service token keeps
				// each grant equal to the endpoint that was actually reviewed.
				"/v1/budget/check": cfg.CellBudgetAccessAudience,
				// The node's side of the work queue. A distinct /v1/node/
				// prefix so ONE Access application covers exactly the node's
				// surface — the human endpoints live under /v1/work and a
				// prefix application over that would have handed a cell token
				// the ability to enqueue work as well as claim it.
			},
			// /v1/node/ is reserved for the execution nodes. Everything under
			// it is a node endpoint, which is what makes a prefix safe here —
			// the human endpoints that CREATE work live under /v1/work.
			PathPrefixAudiences: map[string]string{
				"/v1/node/": cfg.CellWorkAccessAudience,
			},
		}
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           httplog.Middleware(log)(routes(cfg, db, auth, domain.NewResolver(domain.NewStore(db)), log)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Serve until interrupted, then drain in-flight requests.
	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errc:
		log.Error("server failed", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		log.Info("shutdown requested; draining")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownWait)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	log.Info("stopped cleanly")
}

func routes(cfg config.ControlAPI, db *sql.DB, auth authn.Authenticator, resolver authn.Resolver, log *slog.Logger) http.Handler {
	var store *domain.Store
	var ghStore *githubapp.Store
	if db != nil {
		store = domain.NewStore(db)
		ghStore = githubapp.NewStore(db)
	}

	// The GitHub App client, held only here on the control node.
	//
	// Absent credentials are not fatal: the API still serves identity and the
	// registry, and the endpoints that need the App refuse individually. A
	// process that will not start without every integration configured is one
	// that cannot be brought up during an incident.
	var cos *chiefofstaff.Store
	var inbox *attention.Store
	var decisions *approvals.Store
	if db != nil {
		cos = chiefofstaff.NewStore(db)
		inbox = attention.NewStore(db)
		decisions = approvals.NewStore(db)
	}

	var gh *githubapp.Client
	if cfg.GitHubAppID != "" && cfg.GitHubInstallationID != "" && cfg.GitHubPrivateKeyPath != "" {
		key, err := githubapp.LoadPrivateKey(cfg.GitHubPrivateKeyPath)
		if err != nil {
			// Logged loudly rather than ignored: a wrong file mode here is a
			// security finding, not a missing feature.
			log.Error("loading the GitHub App key", "path", cfg.GitHubPrivateKeyPath, "error", err)
		} else {
			gh = &githubapp.Client{
				AppID:          cfg.GitHubAppID,
				InstallationID: cfg.GitHubInstallationID,
				PrivateKeyPEM:  key,
			}
		}
	}
	mux := http.NewServeMux()

	// Liveness answers "is the process running"; it must not depend on
	// downstream services, or a database blip would trigger a restart loop.
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	// Readiness answers "can this instance serve traffic", which means the
	// database is actually reachable — not merely configured.
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if db == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "not_ready", "reason": "no database",
			})
			return
		}
		if err := db.PingContext(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "not_ready", "reason": "database unreachable",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
	})

	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"version": version.Version,
			"commit":  version.Commit,
			"built":   version.BuildDate,
			"env":     string(cfg.Environment),
		})
	})

	// Everything under /v1 requires an authenticated identity. Health and
	// version stay open because a load balancer cannot present a token, and
	// neither reveals anything about the graph.
	authed := http.NewServeMux()

	authed.HandleFunc("GET /v1/me", func(w http.ResponseWriter, r *http.Request) {
		id, ok := authn.FromContext(r.Context())
		if !ok {
			// Unreachable: the middleware fails closed. Present so that a
			// future refactor that removes it fails loudly rather than
			// serving an anonymous session.
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"subject":    id.Subject,
			"email":      id.Email,
			"user_id":    id.UserID,
			"is_service": id.IsService,
		})
	})

	// Mint a git credential for an execution cell.
	//
	// The App private key stays on the control node. It can mint tokens for
	// every installed repository — including the restricted client one — so
	// putting it on an execution node, where untrusted agent code runs, would
	// make a single cell compromise into a compromise of every repository.
	//
	// Instead a cell asks for a token scoped to the ONE repository it is working
	// on, and receives one that expires within the hour (plan section 9.2).
	authed.HandleFunc("POST /v1/integrations/github/installation-token", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())

		// Service tokens only. A human's browser session has no business
		// minting a git credential, and refusing here means a stolen human
		// session cannot be turned into one.
		if !id.IsService {
			log.Warn("token mint refused for a human session", "subject", id.Subject)
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "service callers only"})
			return
		}
		var body struct {
			Repository string `json:"repository"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
			return
		}

		// A repository must be named. Minting an unscoped token here would hand
		// a cell access to every installed repository, which is the exact
		// outcome this endpoint exists to prevent — so the empty case is
		// refused rather than treated as "all".
		repo := strings.TrimSpace(body.Repository)
		if repo == "" || strings.ContainsAny(repo, "/ \t") {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "name exactly one repository, without an owner prefix"})
			return
		}

		// Configuration is checked AFTER the request is validated. A malformed
		// request is malformed whether or not the App happens to be wired up,
		// and answering 503 to it sends the caller looking at the wrong thing.
		if gh == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "github app is not configured"})
			return
		}

		tok, err := gh.InstallationToken(r.Context(), repo)
		if err != nil {
			// The reason is logged, not returned: a caller learning the
			// difference between "not installed" and "App misconfigured" is
			// being told about repositories it cannot see.
			log.Error("minting an installation token", "repository", repo, "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not mint a token"})
			return
		}

		log.Info("minted a scoped installation token",
			"repository", repo, "subject", id.Subject, "expires", tok.ExpiresAt)

		// The token is the response body and nothing else is logged about it.
		writeJSON(w, http.StatusOK, map[string]any{
			"token":      tok.Token,
			"expires_at": tok.ExpiresAt,
			"repository": repo,
		})
	})

	// ---------------------------------------------------------------------
	// The work queue (WP-D2/E3)
	// ---------------------------------------------------------------------
	//
	// The control plane cannot reach an execution node, so a dispatch is not a
	// call — it is a row the node comes and claims. These two endpoints are the
	// node's side of that, and they use the cell service token like every other
	// node-facing endpoint here.

	// Claim the next job for a cell. Returns 204 when there is nothing to do,
	// which is the common case and must not read as an error in a log.
	authed.HandleFunc("POST /v1/node/work/claim", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if !id.IsService {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "service callers only"})
			return
		}
		cell := strings.TrimSpace(r.URL.Query().Get("cell"))
		if cell == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cell is required"})
			return
		}
		if db == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no database"})
			return
		}

		var job work.Job
		var bead, brief sql.NullString
		err := db.QueryRowContext(r.Context(),
			`SELECT id, kind, bead, brief, rig FROM system_claim_work($1)`, cell).
			Scan(&job.ID, &job.Kind, &bead, &brief, &job.Rig)
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			log.Error("claiming work", "cell", cell, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		job.Cell, job.Bead, job.Brief = cell, bead.String, brief.String
		log.Info("work claimed", "cell", cell, "job", job.ID, "kind", job.Kind, "bead", job.Bead)
		writeJSON(w, http.StatusOK, job)
	})

	// Report a finished job.
	authed.HandleFunc("POST /v1/node/work/{id}/result", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if !id.IsService {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "service callers only"})
			return
		}
		var res work.Result
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&res); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
			return
		}
		if db == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no database"})
			return
		}
		var ok bool
		if err := db.QueryRowContext(r.Context(),
			`SELECT system_finish_work($1, $2, $3)`,
			r.PathValue("id"), res.OK, res.Output).Scan(&ok); err != nil {
			log.Error("finishing work", "job", r.PathValue("id"), "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		log.Info("work finished", "job", r.PathValue("id"), "ok", res.OK, "recorded", ok)
		writeJSON(w, http.StatusOK, map[string]any{"recorded": ok})
	})

	// Project beads from a cell's graph into work_refs.
	//
	// work_refs has existed since 0001 as the projection of Beads into the
	// control plane and nothing ever filled it, so the UI could show projects
	// and repositories but never the work. Beads stays canonical; this is a
	// cache so a page can rank and filter without asking every cell.
	authed.HandleFunc("POST /v1/node/work/project", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if !id.IsService {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "service callers only"})
			return
		}
		var payload struct {
			Cell  string `json:"cell"`
			Beads []struct {
				Bead   string `json:"bead"`
				Title  string `json:"title"`
				Kind   string `json:"kind"`
				Status string `json:"status"`
			} `json:"beads"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
			return
		}
		if db == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no database"})
			return
		}
		projected := 0
		for _, b := range payload.Beads {
			if strings.TrimSpace(b.Bead) == "" {
				continue
			}
			var ok bool
			if err := db.QueryRowContext(r.Context(),
				`SELECT system_project_bead($1,$2,$3,$4,$5)`,
				payload.Cell, b.Bead, b.Title, b.Kind, b.Status).Scan(&ok); err != nil {
				log.Error("projecting a bead", "cell", payload.Cell, "bead", b.Bead, "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
				return
			}
			projected++
		}
		writeJSON(w, http.StatusOK, map[string]any{"projected": projected})
	})

	// Agent health, reported by the deterministic witness on each execution
	// node (cmd/witness). The pass is recorded whole — observations included —
	// because the claim being made is that health monitoring needs no
	// inference, and that is only checkable if the quiet decisions are counted
	// alongside the loud ones.
	authed.HandleFunc("POST /v1/agent-health", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())

		// Service tokens only, for the same reason as the token endpoint: this
		// writes to other people's inboxes, and a human browser session has no
		// business doing that.
		if !id.IsService {
			log.Warn("agent health report refused for a human session", "subject", id.Subject)
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "service callers only"})
			return
		}

		var report witness.Report
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&report); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
			return
		}
		if strings.TrimSpace(report.Cell) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cell is required"})
			return
		}
		if len(report.Decisions) == 0 {
			// An empty pass is a legitimate thing to report and not an error,
			// but there is nothing to store.
			writeJSON(w, http.StatusOK, witness.Outcome{})
			return
		}

		out, err := witness.Ingest(r.Context(), db, report)
		if err != nil {
			log.Error("ingesting an agent health report",
				"cell", report.Cell, "repository", report.Repository, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		if out.Unaddressed > 0 {
			log.Error("agent health escalations reached nobody",
				"cell", report.Cell, "repository", report.Repository,
				"unaddressed", out.Unaddressed)
		}
		log.Info("recorded an agent health report",
			"cell", report.Cell, "repository", report.Repository,
			"recorded", out.Recorded, "escalated", out.Escalated, "notified", out.Notified)
		writeJSON(w, http.StatusOK, out)
	})

	// May this bead be dispatched? Asked by scripts/dispatch_bead.sh on the
	// execution node, before it slings anything (wg-qw1).
	//
	// A GET that changes nothing, deliberately. The dispatcher must be able to
	// ask freely — including twice, or after a failure — and an endpoint that
	// recorded an intent would turn "I checked" into state somebody has to
	// clean up when the dispatch never happened.
	//
	// The answer carries its own staleness. Spend arrives by an hourly import,
	// so an allow is only as good as the data behind it, and a caller that
	// cannot see the age will treat an hour-old "you have room" as current.
	authed.HandleFunc("GET /v1/budget/check", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if !id.IsService {
			// Service callers only, like the endpoints beside it. A human
			// browser session asking whether work may start is not a use case,
			// and allowing it would make this a way to enumerate spend.
			log.Warn("budget check refused for a human session", "subject", id.Subject)
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "service callers only"})
			return
		}

		bead := strings.TrimSpace(r.URL.Query().Get("bead"))
		if bead == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bead is required"})
			return
		}
		cell := strings.TrimSpace(r.URL.Query().Get("cell"))

		if db == nil {
			// Refuse rather than allow. A budget check that fails open is a
			// budget check that stops existing the moment the database is
			// unreachable, which is not a state anybody would notice.
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"allow": false, "reason": "the budget cannot be checked: no database"})
			return
		}

		status, err := budget.Read(r.Context(), db, bead, cell)
		if err != nil {
			log.Error("reading a budget", "bead", bead, "cell", cell, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"allow": false, "reason": "the budget cannot be checked: internal error"})
			return
		}

		decision := budget.Decide(status, budget.PolicyFromEnv())
		log.Info("budget check", "bead", bead, "cell", cell,
			"allow", decision.Allow, "subject", status.SubjectKind,
			"spent", status.SpentCents, "limit", status.DailyCents,
			"staleness_seconds", status.StalenessSeconds)
		writeJSON(w, http.StatusOK, decision)
	})

	// ---------------------------------------------------------------------
	// Work, for a person (WP-F1)
	// ---------------------------------------------------------------------

	// Everything there is to see about the work: the beads, whether an agent is
	// queued or running on one, and what each has cost.
	authed.HandleFunc("GET /v1/work", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "a user is required"})
			return
		}
		if db == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no database"})
			return
		}
		cell := nullableParam(r.URL.Query().Get("cell"))

		type item struct {
			Bead       string `json:"bead"`
			Title      string `json:"title"`
			Kind       string `json:"kind"`
			Status     string `json:"status"`
			Cell       string `json:"cell"`
			Project    string `json:"project,omitempty"`
			LastSeen   string `json:"last_seen"`
			QueueState string `json:"queue_state,omitempty"`
			QueuedAt   string `json:"queued_at,omitempty"`
			SpentCents string `json:"spent_cents"`
			Requests   int64  `json:"requests"`
		}
		rows, err := db.QueryContext(r.Context(),
			`SELECT bead, coalesce(title,''), coalesce(kind,''), coalesce(status,''),
			        coalesce(cell,''), coalesce(project,''), last_seen,
			        coalesce(queue_state,''), queued_at, spent_cents::text, requests
			   FROM system_work_overview($1)`, cell)
		if err != nil {
			log.Error("listing work", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		defer rows.Close()

		out := []item{}
		for rows.Next() {
			var it item
			var lastSeen sql.NullTime
			var queuedAt sql.NullTime
			if err := rows.Scan(&it.Bead, &it.Title, &it.Kind, &it.Status, &it.Cell,
				&it.Project, &lastSeen, &it.QueueState, &queuedAt, &it.SpentCents, &it.Requests); err != nil {
				log.Error("scanning work", "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
				return
			}
			if lastSeen.Valid {
				it.LastSeen = lastSeen.Time.UTC().Format(time.RFC3339)
			}
			if queuedAt.Valid {
				it.QueuedAt = queuedAt.Time.UTC().Format(time.RFC3339)
			}
			out = append(out, it)
		}
		writeJSON(w, http.StatusOK, map[string]any{"work": out})
	})

	// The queue, including plan jobs, which have no bead to hang off until they
	// have produced some.
	authed.HandleFunc("GET /v1/work/queue", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "a user is required"})
			return
		}
		if db == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no database"})
			return
		}
		rows, err := db.QueryContext(r.Context(),
			`SELECT id, kind, cell, rig, coalesce(bead,''), coalesce(brief,''),
			        status, created_at, finished_at, coalesce(result,'')
			   FROM system_queue_overview($1)`, nullableParam(r.URL.Query().Get("cell")))
		if err != nil {
			log.Error("listing the queue", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		defer rows.Close()

		type job struct {
			ID         string `json:"id"`
			Kind       string `json:"kind"`
			Cell       string `json:"cell"`
			Rig        string `json:"rig"`
			Bead       string `json:"bead,omitempty"`
			Brief      string `json:"brief,omitempty"`
			Status     string `json:"status"`
			CreatedAt  string `json:"created_at"`
			FinishedAt string `json:"finished_at,omitempty"`
			Result     string `json:"result,omitempty"`
		}
		out := []job{}
		for rows.Next() {
			var j job
			var created time.Time
			var finished sql.NullTime
			if err := rows.Scan(&j.ID, &j.Kind, &j.Cell, &j.Rig, &j.Bead, &j.Brief,
				&j.Status, &created, &finished, &j.Result); err != nil {
				log.Error("scanning the queue", "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
				return
			}
			j.CreatedAt = created.UTC().Format(time.RFC3339)
			if finished.Valid {
				j.FinishedAt = finished.Time.UTC().Format(time.RFC3339)
			}
			out = append(out, j)
		}
		writeJSON(w, http.StatusOK, map[string]any{"queue": out})
	})

	// Turn a brief into beads.
	//
	// Enqueued rather than run: the node claims it. That is not only an
	// architectural necessity — it also means this returns immediately and the
	// page can show the job moving through queued, running and done, rather than
	// holding a request open for however long an agent takes.
	authed.HandleFunc("POST /v1/work/plan", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "a user is required"})
			return
		}
		var payload struct {
			Brief string `json:"brief"`
			Cell  string `json:"cell"`
			Rig   string `json:"rig"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
			return
		}
		if strings.TrimSpace(payload.Brief) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a brief is required"})
			return
		}
		if strings.TrimSpace(payload.Cell) == "" {
			payload.Cell = "oss"
		}
		if db == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no database"})
			return
		}
		var jobID string
		if err := db.QueryRowContext(r.Context(),
			`SELECT system_enqueue_work('plan', $1, $2, NULL, $3, $4)`,
			payload.Cell, payload.Rig, payload.Brief, id.UserID).Scan(&jobID); err != nil {
			log.Error("enqueueing a plan", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		log.Info("plan enqueued", "job", jobID, "cell", payload.Cell, "by", id.UserID)
		writeJSON(w, http.StatusAccepted, map[string]any{"job": jobID, "status": "queued"})
	})

	// Send one bead to an agent.
	authed.HandleFunc("POST /v1/work/{bead}/dispatch", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "a user is required"})
			return
		}
		var payload struct {
			Cell string `json:"cell"`
			Rig  string `json:"rig"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&payload)
		if strings.TrimSpace(payload.Cell) == "" {
			payload.Cell = "oss"
		}
		if db == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no database"})
			return
		}

		bead := r.PathValue("bead")

		// The budget is checked HERE, before the job is queued, rather than
		// only on the node. A refusal a person can see when they press the
		// button is worth more than one they find in a journal afterwards, and
		// the node checks again anyway — that second check is what actually
		// protects the money.
		if status, err := budget.Read(r.Context(), db, bead, payload.Cell); err == nil {
			if d := budget.Decide(status, budget.PolicyFromEnv()); !d.Allow {
				log.Warn("dispatch refused by budget", "bead", bead, "reason", d.Reason)
				writeJSON(w, http.StatusPaymentRequired, map[string]any{
					"error": d.Reason, "budget": d.Status,
				})
				return
			}
		}

		var jobID string
		if err := db.QueryRowContext(r.Context(),
			`SELECT system_enqueue_work('work', $1, $2, $3, NULL, $4)`,
			payload.Cell, payload.Rig, bead, id.UserID).Scan(&jobID); err != nil {
			log.Error("enqueueing work", "bead", bead, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		log.Info("work enqueued", "job", jobID, "bead", bead, "by", id.UserID)
		writeJSON(w, http.StatusAccepted, map[string]any{"job": jobID, "bead": bead, "status": "queued"})
	})

	// The registry. Every read runs inside a transaction carrying the caller's
	// user ID, so row-level security decides what comes back — the handler does
	// no filtering of its own, and cannot forget to.
	authed.HandleFunc("GET /v1/projects", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if store == nil || id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		projects, err := store.ListProjects(r.Context(), id.UserID)
		if err != nil {
			log.Error("listing projects", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		if projects == nil {
			projects = []domain.ProjectSummary{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
	})

	authed.HandleFunc("GET /v1/projects/{slug}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if store == nil || id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		p, err := store.ProjectBySlug(r.Context(), id.UserID, r.PathValue("slug"))
		if errors.Is(err, domain.ErrNotFound) {
			// Deliberately the same response whether the project does not exist
			// or the caller may not see it. Distinguishing them would confirm a
			// restricted engagement exists, and its name alone is confidential.
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		if err != nil {
			log.Error("reading project", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, p)
	})

	// The chief-of-staff questions.
	//
	// The supported questions are listed by the API rather than hardcoded in the
	// interface, so an unsupported one is refused in the same place for every
	// client — including a future model-backed one.
	authed.HandleFunc("GET /v1/ask", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if cos == nil || id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		question := r.URL.Query().Get("q")
		if question == "" {
			writeJSON(w, http.StatusOK, map[string]any{"questions": chiefofstaff.Questions})
			return
		}
		answer, err := cos.Ask(r.Context(), id.UserID, question)
		if err != nil {
			// An unsupported question is a client error, and the reply names
			// what IS answerable rather than inventing something.
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":     err.Error(),
				"questions": chiefofstaff.Questions,
			})
			return
		}
		writeJSON(w, http.StatusOK, answer)
	})

	// The attention inbox.
	authed.HandleFunc("GET /v1/inbox", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if inbox == nil || id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		items, err := inbox.Inbox(r.Context(), id.UserID)
		if err != nil {
			log.Error("reading the inbox", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		// Enveloped, matching /v1/projects. Two conventions in one API is a
		// trap for every future client: this exact mismatch — an envelope on
		// one endpoint and a bare array on another — is what made the projects
		// table render empty while the server was returning all three.
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	})

	// Selective descent: branches with a few candidates each, and a reason when
	// a branch has none.
	authed.HandleFunc("GET /v1/inbox/branches", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if inbox == nil || id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		branches, err := inbox.Descend(r.Context(), id.UserID)
		if err != nil {
			log.Error("descending the inbox", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"branches": branches})
	})

	// Decide an approval.
	//
	// Refusals carry the policy that stopped the caller. The status is 403 for
	// every policy refusal, so probing this endpoint reveals nothing about
	// which rule applies to a request the caller cannot see.
	authed.HandleFunc("POST /v1/approvals/{id}/decide", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if decisions == nil || id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		var body struct {
			Approve    bool   `json:"approve"`
			Reason     string `json:"reason"`
			SeenDigest string `json:"seen_digest"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
			return
		}

		err := decisions.Decide(r.Context(), approvals.Decision{
			RequestID:  r.PathValue("id"),
			UserID:     id.UserID,
			Approve:    body.Approve,
			Reason:     body.Reason,
			SeenDigest: body.SeenDigest,
		})
		var refusal *approvals.Refusal
		if errors.As(err, &refusal) {
			log.Info("approval refused", "policy", refusal.Policy,
				"request", r.PathValue("id"), "subject", id.Subject)
			writeJSON(w, http.StatusForbidden, refusal)
			return
		}
		if err != nil {
			log.Error("deciding an approval", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "recorded"})
	})

	// The project page. Returns the summary, its repositories with projected
	// pull requests, and derived signals that each carry their evidence.
	authed.HandleFunc("GET /v1/projects/{slug}/detail", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if store == nil || id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		d, err := store.ProjectDetailBySlug(r.Context(), id.UserID, r.PathValue("slug"))
		if errors.Is(err, domain.ErrNotFound) {
			// Same response whether it does not exist or the caller may not see
			// it, for the same reason as the summary endpoint.
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		if err != nil {
			log.Error("reading project detail", "slug", r.PathValue("slug"), "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, d)
	})

	authed.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "not implemented",
			"path":  r.URL.Path,
		})
	})

	mux.Handle("/v1/", authn.Middleware(auth, resolver, log)(authed))

	// The GitHub webhook is deliberately OUTSIDE the authenticated mux.
	//
	// GitHub cannot complete a Cloudflare Access challenge, so this endpoint
	// authenticates itself: every request is verified against the shared secret
	// with a constant-time comparison before the payload is parsed. It is
	// reachable by anyone who learns the URL, which is why the body size is
	// bounded and nothing is decoded before the signature checks out.
	// Registered on the ROOT mux, not the authenticated one. Go's ServeMux
	// prefers the more specific pattern, so this wins over the "/v1/" handler
	// and never reaches the authentication middleware.
	//
	// This is the single documented exception to "everything under /v1 requires
	// an identity", and cmd/control-api asserts it is the only one — an
	// exception that is tested is a decision; an untested one is a hole.
	mux.HandleFunc("POST /v1/integrations/github/webhook", func(w http.ResponseWriter, r *http.Request) {
		delivery, err := githubapp.VerifyWebhook(r,
			[]byte(cfg.GitHubWebhookSecret), []byte(cfg.GitHubWebhookSecretPrevious))
		if err != nil {
			// One response for every rejection. Distinguishing a bad signature
			// from a missing one tells a forger what to change.
			log.Warn("webhook rejected", "error", err, "remote", r.Header.Get("Cf-Connecting-Ip"))
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}

		if ghStore != nil {
			switch err := ghStore.RecordDelivery(r.Context(), delivery); {
			case errors.Is(err, githubapp.ErrDuplicateDelivery):
				// A retry. Acknowledge so GitHub stops resending, but do not
				// process it again.
				log.Info("duplicate delivery ignored", "delivery", delivery.ID, "event", delivery.Event)
				writeJSON(w, http.StatusOK, map[string]any{"status": "duplicate"})
				return
			case err != nil:
				// Do NOT acknowledge: a 500 makes GitHub retry, which is what
				// should happen when the receipt could not be stored.
				log.Error("recording delivery", "delivery", delivery.ID, "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
				return
			}
		}

		// Acknowledge quickly, then project (plan section 9.3). GitHub times
		// out a delivery in ten seconds, and a projection that touches several
		// rows must not be what decides whether the event is acknowledged --
		// the receipt is already durable, so a slow projection can be retried
		// from it rather than by asking GitHub to resend.
		log.Info("webhook received", "delivery", delivery.ID, "event", delivery.Event, "bytes", len(delivery.Body))
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted"})

		if db == nil {
			return
		}
		body, event, id := delivery.Body, delivery.Event, delivery.ID
		go func() {
			// A detached context: the request is already answered, so the
			// projection must not be cancelled when the connection closes.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
			defer cancel()

			var applied bool
			var err error
			switch event {
			case "pull_request":
				applied, err = githubapp.ProjectPullRequest(ctx, db, body)
			case "check_suite":
				applied, err = githubapp.ProjectCheckSuite(ctx, db, body)
			default:
				return
			}

			switch {
			case errors.Is(err, githubapp.ErrRepositoryNotRegistered):
				// Expected: the App may be installed on repositories no project
				// claims. Deliberately NOT marked processed — the receipt stays
				// replayable, so registering the repository later can recover
				// the history rather than starting from whatever arrives next.
				log.Info("event for an unregistered repository", "delivery", id, "event", event)
				return
			case err != nil:
				log.Error("projecting event", "delivery", id, "event", event, "error", err)
				return
			}

			if ghStore != nil {
				if err := ghStore.MarkProcessed(ctx, id); err != nil {
					log.Error("marking delivery processed", "delivery", id, "error", err)
				}
			}
			log.Info("event projected", "delivery", id, "event", event, "applied", applied)
		}()
	})

	// The single-page application, when one is embedded.
	//
	// Static assets carry no project data — everything the page shows comes
	// from /v1, which authenticates every request — and the hostname itself
	// sits behind Cloudflare Access, so a person loading the page has already
	// proven who they are.
	if ui := webui.Handler(); ui != nil {
		mux.Handle("/", ui)
		log.Info("serving the web interface")
	} else {
		// No build embedded. The API still serves /v1, deliberately: an
		// operator working an incident must not be blocked by a missing
		// frontend bundle.
		log.Warn("no web interface embedded; serving the API only")
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		})
	}

	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// nullableParam turns an absent query parameter into a SQL NULL, so a filter
// that was not asked for does not become a filter for the empty string.
func nullableParam(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}
