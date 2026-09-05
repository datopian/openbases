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
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/workgraph/internal/apispec"
	"github.com/datopian/workgraph/internal/approvals"
	"github.com/datopian/workgraph/internal/attention"
	"github.com/datopian/workgraph/internal/authn"
	"github.com/datopian/workgraph/internal/authz"
	"github.com/datopian/workgraph/internal/budget"
	"github.com/datopian/workgraph/internal/chiefofstaff"
	"github.com/datopian/workgraph/internal/config"
	"github.com/datopian/workgraph/internal/dispatchroute"
	"github.com/datopian/workgraph/internal/domain"
	"github.com/datopian/workgraph/internal/events"
	"github.com/datopian/workgraph/internal/githubapp"
	"github.com/datopian/workgraph/internal/httplog"
	"github.com/datopian/workgraph/internal/idempotency"
	"github.com/datopian/workgraph/internal/tokens"
	"github.com/datopian/workgraph/internal/version"
	"github.com/datopian/workgraph/internal/webui"
	"github.com/datopian/workgraph/internal/witness"
	"github.com/datopian/workgraph/internal/work"
	"github.com/datopian/workgraph/internal/workspace"
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

	// Personal API tokens, second in the chain (wg-p4h.3).
	//
	// Access first, because a browser session is the common case and a request
	// carrying an Access assertion should be judged by it — a revoked session
	// must not be rescued by a token in the same request. The token link is
	// reached only when no Access assertion is present at all, which is exactly
	// the case a tool produces.
	//
	// Without a database there is no token store, so the chain is Access alone
	// and a bearer token is refused rather than silently ignored.
	if db != nil {
		auth = authn.Chain{
			authn.AccessOrNotRecognised{Inner: auth},
			&authn.BearerAuthenticator{
				Lookup: tokens.NewLookup(tokens.NewStore(db)),
				Log:    log,
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
	var apiTokens *tokens.Store
	var idem *idempotency.Store
	var eventLog *events.Store
	limiter := tokens.NewLimiter()
	var grants *authz.Store
	if db != nil {
		cos = chiefofstaff.NewStore(db)
		inbox = attention.NewStore(db)
		decisions = approvals.NewStore(db)
		apiTokens = tokens.NewStore(db)
		idem = idempotency.NewStore(db)
		eventLog = events.NewStore(db)
		grants = authz.NewStore(db)
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

	// The contract, served unauthenticated (wg-p4h.5).
	//
	// Outside the /v1/ mux on purpose. A client that must authenticate in order
	// to discover how to authenticate is a bad first five minutes, and the
	// document contains no secrets — it describes shapes, not data.
	mux.HandleFunc("GET /v1/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(apispec.OpenAPI)
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
		var bead, brief, project sql.NullString
		err := db.QueryRowContext(r.Context(),
			`SELECT id, kind, bead, brief, rig, project FROM system_claim_work($1)`, cell).
			Scan(&job.ID, &job.Kind, &bead, &brief, &job.Rig, &project)
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
		job.Project = project.String
		log.Info("work claimed", "cell", cell, "job", job.ID, "kind", job.Kind,
			"bead", job.Bead, "project", job.Project)
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

	// A rig reports which repository it holds (wg-ugb).
	//
	// Reported by the node rather than configured centrally, because the node
	// is where the truth is: a rig is a working tree and its git remote says
	// what it is a checkout of. Two statements of one fact would drift, and the
	// one that drifts is the one nobody looks at.
	authed.HandleFunc("POST /v1/node/rigs", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if !id.IsService {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "service callers only"})
			return
		}
		var payload struct {
			Cell     string `json:"cell"`
			Rig      string `json:"rig"`
			Provider string `json:"provider"`
			Owner    string `json:"owner"`
			Name     string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
			return
		}
		if db == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no database"})
			return
		}
		var ok bool
		if err := db.QueryRowContext(r.Context(),
			`SELECT system_register_rig($1,$2,$3,$4,$5)`,
			payload.Cell, payload.Rig,
			nullableParam(payload.Provider), nullableParam(payload.Owner),
			nullableParam(payload.Name)).Scan(&ok); err != nil {
			log.Error("registering a rig", "cell", payload.Cell, "rig", payload.Rig, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"registered": true})
	})

	// Which rigs this cell should have, one per repository its projects hold.
	//
	// Read by the playbook, which provisions what is missing with `gt rig add`.
	// Provisioning rather than polling: creating a rig clones a repository and
	// seeds agent infrastructure, which is minutes of work and gigabytes of
	// disk, and that belongs in a deploy somebody ran rather than in a
	// fifteen-second dispatcher pass that would start cloning the moment a
	// repository was attached from a phone.
	authed.HandleFunc("GET /v1/node/rigs/wanted", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if !id.IsService {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "service callers only"})
			return
		}
		cell := strings.TrimSpace(r.URL.Query().Get("cell"))
		if cell == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a cell is required"})
			return
		}
		if db == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no database"})
			return
		}

		type wanted struct {
			Rig      string `json:"rig"`
			Provider string `json:"provider"`
			Owner    string `json:"owner"`
			Name     string `json:"name"`
			CloneURL string `json:"clone_url"`
			Prefix   string `json:"prefix"`
			Project  string `json:"project"`
			Held     bool   `json:"held"`
		}
		out := []wanted{}
		rows, err := db.QueryContext(r.Context(),
			`SELECT rig, provider, owner, name, clone_url, prefix, project, held
			   FROM system_rigs_wanted($1)`, cell)
		if err != nil {
			log.Error("listing wanted rigs", "cell", cell, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		defer rows.Close()
		for rows.Next() {
			var v wanted
			if err := rows.Scan(&v.Rig, &v.Provider, &v.Owner, &v.Name,
				&v.CloneURL, &v.Prefix, &v.Project, &v.Held); err != nil {
				log.Error("reading a wanted rig", "cell", cell, "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
				return
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			log.Error("listing wanted rigs", "cell", cell, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"cell": cell, "rigs": out})
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
				Bead      string   `json:"bead"`
				Title     string   `json:"title"`
				Kind      string   `json:"kind"`
				Status    string   `json:"status"`
				Labels    []string `json:"labels"`
				Comment   string   `json:"comment"`
				CommentAt string   `json:"comment_at"`
				CommentBy string   `json:"comment_by"`
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
				`SELECT system_project_bead($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				payload.Cell, b.Bead, b.Title, b.Kind, b.Status,
				beadLabels(b.Labels),
				nullableParam(b.Comment), nullableTime(b.CommentAt),
				nullableParam(b.CommentBy)).Scan(&ok); err != nil {
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
		// Inside authz.WithUser, because system_work_overview is SECURITY
		// DEFINER and filters on current_app_user(). Querying db directly --
		// which this did -- sets no identity, so the function saw no user and
		// its filter would return nothing. The endpoint checked that a user
		// was authenticated and then asked the database as nobody.
		out := []item{}
		err := authz.WithUser(r.Context(), db, id.UserID, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(r.Context(),
				`SELECT bead, coalesce(title,''), coalesce(kind,''), coalesce(status,''),
				        coalesce(cell,''), coalesce(project,''), last_seen,
				        coalesce(queue_state,''), queued_at, spent_cents::text, requests
				   FROM system_work_overview($1)`, cell)
			if err != nil {
				return err
			}
			defer rows.Close()

			for rows.Next() {
				var it item
				var lastSeen sql.NullTime
				var queuedAt sql.NullTime
				if err := rows.Scan(&it.Bead, &it.Title, &it.Kind, &it.Status, &it.Cell,
					&it.Project, &lastSeen, &it.QueueState, &queuedAt, &it.SpentCents, &it.Requests); err != nil {
					return err
				}
				if lastSeen.Valid {
					it.LastSeen = lastSeen.Time.UTC().Format(time.RFC3339)
				}
				if queuedAt.Valid {
					it.QueuedAt = queuedAt.Time.UTC().Format(time.RFC3339)
				}
				out = append(out, it)
			}
			return rows.Err()
		})
		if err != nil {
			log.Error("listing work", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
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
		// Same reason as GET /v1/work: system_queue_overview filters on
		// current_app_user(), so the query has to carry an identity.
		out := []job{}
		err := authz.WithUser(r.Context(), db, id.UserID, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(r.Context(),
				`SELECT id, kind, cell, rig, coalesce(bead,''), coalesce(brief,''),
				        status, created_at, finished_at, coalesce(result,'')
				   FROM system_queue_overview($1)`, nullableParam(r.URL.Query().Get("cell")))
			if err != nil {
				return err
			}
			defer rows.Close()

			for rows.Next() {
				var j job
				var created time.Time
				var finished sql.NullTime
				if err := rows.Scan(&j.ID, &j.Kind, &j.Cell, &j.Rig, &j.Bead, &j.Brief,
					&j.Status, &created, &finished, &j.Result); err != nil {
					return err
				}
				j.CreatedAt = created.UTC().Format(time.RFC3339)
				if finished.Valid {
					j.FinishedAt = finished.Time.UTC().Format(time.RFC3339)
				}
				out = append(out, j)
			}
			return rows.Err()
		})
		if err != nil {
			log.Error("listing the queue", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
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
			Brief   string `json:"brief"`
			Cell    string `json:"cell"`
			Rig     string `json:"rig"`
			Project string `json:"project"`
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
		// id.UserID, never a user from the body: system_enqueue_work checks
		// THAT user's membership of the project, so passing anything the
		// caller supplied would make the check check nothing.
		if err := db.QueryRowContext(r.Context(),
			`SELECT system_enqueue_work('plan', $1, $2, NULL, $3, $4, $5)`,
			payload.Cell, payload.Rig, payload.Brief, id.UserID,
			nullableParam(strings.TrimSpace(payload.Project))).Scan(&jobID); err != nil {
			// The function raises for an unknown project and for a requester
			// who is not a member. Both are the caller's mistake and both are
			// safe to name -- "not a member of project x" tells them nothing
			// they did not just assert -- so this is a 400 with the reason
			// rather than a 500 with "internal error".
			msg := err.Error()
			if strings.Contains(msg, "no project with the slug") ||
				strings.Contains(msg, "is not a member of project") {
				log.Info("plan refused", "by", id.UserID, "project", payload.Project, "reason", msg)
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": "that project is not one you can file work into"})
				return
			}
			log.Error("enqueueing a plan", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		log.Info("plan enqueued", "job", jobID, "cell", payload.Cell,
			"project", payload.Project, "by", id.UserID)
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
		// Route the dispatch to a rig that can actually do the work, or refuse
		// it (wg-ugb).
		//
		// Three PortalJS beads were dispatched on 4 September and ran against
		// datopian/workgraph-agent-sandbox, because this handler defaulted the
		// cell to "oss" and passed the rig through unset, and the dispatcher
		// then fell back to its own default rig. The agent looked for PortalJS
		// source, correctly found none, and said so — after 48 model calls and
		// about 78 cents.
		//
		// So: if the bead belongs to a project, the rig must hold one of that
		// project's repositories. A refusal costs nothing; running somewhere
		// disposable and reporting `done` cost money and looked like success.
		//
		// A NAMED rig is checked too, and that is the point of this being
		// written as one path rather than two. The first version skipped the
		// whole check whenever the caller named a rig, so `{"rig":"sandbox"}`
		// reproduced the exact failure the check exists to prevent -- a
		// PortalJS bead running in a disposable sandbox, reporting done, having
		// found no PortalJS source. A caller naming a rig is choosing between
		// the rigs that can do the work, not opting out of the question.
		rig, why, err := dispatchroute.For(r.Context(), db, bead, payload.Cell, payload.Rig)
		switch {
		case err != nil:
			log.Error("routing a dispatch", "bead", bead, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		case why != "":
			// 409 rather than 400: the request is well formed and the
			// registry cannot honour it yet. The message says what is
			// missing, because "no rig" is fixed by registering a
			// repository or provisioning a rig, and the person reading it
			// needs to know which.
			log.Info("dispatch refused for want of a rig", "bead", bead, "reason", why)
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": why,
				"code":  "no_rig_for_project",
			})
			return
		default:
			payload.Rig = rig
		}

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

	// Personal API tokens (wg-p4h.2). The credential a person gives a tool.
	//
	// tokenMinter guards the one rule that cannot be expressed as a scope: a
	// token may not mint a token. A leaked credential that can produce its own
	// successor makes revocation meaningless, because revoking the one you know
	// about leaves the one it created. So minting requires an interactive Access
	// session, and the refusal is written now — before wg-p4h.3 wires the
	// authenticator that would make it reachable — because a rule added after
	// the path exists is a rule that was briefly absent.
	tokenMinter := func(w http.ResponseWriter, r *http.Request) (authn.Identity, bool) {
		id, _ := authn.FromContext(r.Context())
		if apiTokens == nil || id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return id, false
		}
		if id.ViaToken() {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "a token cannot manage tokens",
				"code":  "token_self_service_forbidden",
				"hint":  "mint and revoke tokens from the interface, with an Access session",
			})
			return id, false
		}
		return id, true
	}

	// ---------------------------------------------------------------------
	// The device authorization grant (wg-8la, RFC 8628)
	// ---------------------------------------------------------------------
	//
	// Minting needs an interactive session, which is right and which left a
	// sandboxed agent nowhere to go: it cannot be given an environment
	// variable, cannot open a browser, and must not be handed a credential
	// through a chat transcript. This adds a path without weakening the rule.
	// The agent asks, a PERSON approves in a browser, the agent collects.
	//
	// Start and poll are on `mux`, not `authed`: a client with no credential
	// is exactly who calls them, so requiring one would defeat the purpose.
	// Approval is on `authed` behind the same tokenMinter guard as minting,
	// because approving IS minting with an extra step.

	mux.HandleFunc("POST /v1/device/code", func(w http.ResponseWriter, r *http.Request) {
		if apiTokens == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "tokens are not configured"})
			return
		}
		var body struct {
			ClientLabel string   `json:"client_label"`
			Scopes      []string `json:"scopes"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed body"})
			return
		}
		if cfg.AppBaseURL == "" {
			// Refused rather than guessed. The verification URI is what a
			// person is asked to open and trust, so an unconfigured one is a
			// missing prerequisite and not a default to invent.
			log.Error("a device grant was requested but WG_APP_BASE_URL is not set")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "the device flow is not configured on this deployment",
				"hint":  "set WG_APP_BASE_URL to the interface's base URL"})
			return
		}
		grant, err := apiTokens.StartDevice(r.Context(), body.ClientLabel, body.Scopes, cfg.AppBaseURL)
		if err != nil {
			// A refused scope is the caller's mistake and safe to name: they
			// asked for it, so telling them it is ungrantable reveals nothing.
			if errors.Is(err, tokens.ErrProtectedScope) || strings.Contains(err.Error(), "cannot hold that action") {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": err.Error(), "code": "scope_refused"})
				return
			}
			// An unknown scope is a typo, and answering "internal error" to a
			// typo is the dead end this whole flow exists to remove: a caller
			// with no credential guesses a plausible name, learns nothing, and
			// stops. Name it and say what the real ones are.
			if errors.Is(err, tokens.ErrUnknownScope) {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error":     err.Error(),
					"code":      "scope_unknown",
					"grantable": tokens.GrantableScopes()})
				return
			}
			log.Error("starting a device grant", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		log.Info("device grant started", "client", body.ClientLabel, "user_code", grant.UserCode)
		writeJSON(w, http.StatusOK, grant)
	})

	mux.HandleFunc("POST /v1/device/token", func(w http.ResponseWriter, r *http.Request) {
		if apiTokens == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "tokens are not configured"})
			return
		}
		var body struct {
			DeviceCode string `json:"device_code"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed body"})
			return
		}

		secret, t, err := apiTokens.RedeemDevice(r.Context(), body.DeviceCode)
		switch {
		case errors.Is(err, tokens.ErrDevicePending):
			// 400 with a code, per RFC 8628, rather than an error status a
			// client would give up on. Pending is the NORMAL answer for most
			// of this flow's life.
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": tokens.DevicePending})
			return
		case errors.Is(err, tokens.ErrDeviceExpired):
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": tokens.DeviceExpired})
			return
		case errors.Is(err, tokens.ErrDeviceInvalid):
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": tokens.DeviceInvalid})
			return
		case err != nil:
			log.Error("redeeming a device grant", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}

		log.Info("device grant redeemed", "token", t.ID, "user", t.UserID, "expires", t.ExpiresAt)
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": secret,
			"token_type":   "Bearer",
			"expires_in":   int(time.Until(t.ExpiresAt).Seconds()),
			"scope":        strings.Join(t.Scopes, " "),
		})
	})

	// What the approval page shows. Behind the session guard, because the page
	// is only ever reached by a person who has already logged in.
	authed.HandleFunc("GET /v1/device/request", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := tokenMinter(w, r); !ok {
			return
		}
		req, err := apiTokens.LookupDevice(r.Context(), r.URL.Query().Get("code"))
		if err != nil {
			// One answer for "no such code", "expired" and "already used", so
			// a mistyped code teaches nobody which codes exist.
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "that code is not valid"})
			return
		}
		writeJSON(w, http.StatusOK, req)
	})

	authed.HandleFunc("POST /v1/device/approve", func(w http.ResponseWriter, r *http.Request) {
		id, ok := tokenMinter(w, r)
		if !ok {
			return
		}
		var body struct {
			UserCode string `json:"user_code"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed body"})
			return
		}
		approved, err := apiTokens.ApproveDevice(r.Context(), body.UserCode, id.UserID)
		if err != nil {
			log.Error("approving a device grant", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		if !approved {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "that code is not valid"})
			return
		}
		log.Info("device grant approved", "by", id.UserID)
		writeJSON(w, http.StatusOK, map[string]any{"approved": true})
	})

	authed.HandleFunc("POST /v1/tokens", func(w http.ResponseWriter, r *http.Request) {
		id, ok := tokenMinter(w, r)
		if !ok {
			return
		}
		var body struct {
			Label     string   `json:"label"`
			Scopes    []string `json:"scopes"`
			ExpiresIn string   `json:"expires_in"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
			return
		}
		d, err := time.ParseDuration(body.ExpiresIn)
		if err != nil || d <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "expires_in must be a positive duration, for example \"720h\"",
				"code":  "invalid_expiry",
			})
			return
		}
		secret, t, err := apiTokens.Mint(r.Context(), id.UserID, body.Label, body.Scopes,
			time.Now().Add(d), "interface")
		if errors.Is(err, tokens.ErrProtectedScope) || errors.Is(err, tokens.ErrLifetime) {
			// A refusal the caller can act on, unlike the generic 400 above.
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": err.Error(), "code": "scope_or_lifetime_refused",
			})
			return
		}
		// Same reasoning as the device grant: a misspelled scope is the
		// caller's own input, so it is safe to name and useless to hide.
		if errors.Is(err, tokens.ErrUnknownScope) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":     err.Error(),
				"code":      "scope_unknown",
				"grantable": tokens.GrantableScopes(),
			})
			return
		}
		if err != nil {
			log.Error("minting a token", "error", err, "user", id.UserID)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		// The only time the secret exists outside the caller's hands. It is not
		// logged here or anywhere: the digest is all the server keeps.
		log.Info("token minted", "user", id.UserID, "token", t.ID, "label", t.Label,
			"scopes", t.Scopes, "expires", t.ExpiresAt.UTC().Format(time.RFC3339))
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      secret,
			"id":         t.ID,
			"label":      t.Label,
			"scopes":     t.Scopes,
			"expires_at": t.ExpiresAt.UTC().Format(time.RFC3339),
			"warning":    "this is the only time the token is shown; store it now",
		})
	})

	authed.HandleFunc("GET /v1/tokens", func(w http.ResponseWriter, r *http.Request) {
		id, ok := tokenMinter(w, r)
		if !ok {
			return
		}
		list, err := apiTokens.List(r.Context(), id.UserID)
		if err != nil {
			log.Error("listing tokens", "error", err, "user", id.UserID)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		// Enveloped, matching every other collection in this API.
		out := make([]map[string]any, 0, len(list))
		for _, t := range list {
			item := map[string]any{
				"id":         t.ID,
				"label":      t.Label,
				"scopes":     t.Scopes,
				"created_at": t.CreatedAt.UTC().Format(time.RFC3339),
				"expires_at": t.ExpiresAt.UTC().Format(time.RFC3339),
				"live":       t.Live(time.Now()),
			}
			if t.LastUsedAt != nil {
				item["last_used_at"] = t.LastUsedAt.UTC().Format(time.RFC3339)
			}
			if t.RevokedAt != nil {
				item["revoked_at"] = t.RevokedAt.UTC().Format(time.RFC3339)
			}
			out = append(out, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
	})

	authed.HandleFunc("DELETE /v1/tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := tokenMinter(w, r)
		if !ok {
			return
		}
		err := apiTokens.Revoke(r.Context(), id.UserID, r.PathValue("id"))
		if errors.Is(err, tokens.ErrNotFound) {
			// Someone else's token and a token that does not exist are the same
			// answer: row-level security means this handler never learns which.
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such token"})
			return
		}
		if err != nil {
			log.Error("revoking a token", "error", err, "user", id.UserID)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		log.Info("token revoked", "user", id.UserID, "token", r.PathValue("id"))
		writeJSON(w, http.StatusOK, map[string]any{"status": "revoked"})
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

	// What the platform noticed about a project, from Google Workspace (WP-H3).
	//
	// Exists because "the kick-off was captured" and "you can see that it was"
	// were different things: the event tables had row-level security forced
	// with no policies at all, so the only read path was the SECURITY DEFINER
	// functions the reconciler uses, and no person could reach them.
	authed.HandleFunc("GET /v1/projects/{slug}/events", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if store == nil || id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		events, err := store.ProjectEventsBySlug(r.Context(), id.UserID, r.PathValue("slug"), limit)
		if errors.Is(err, domain.ErrNotFound) {
			// Not found, not forbidden, and the same answer either way -- a
			// caller who may not see a restricted project must not learn it
			// exists by being told they are not allowed to look at it.
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		if err != nil {
			log.Error("reading project events", "slug", r.PathValue("slug"), "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, events)
	})

	// The knowledge review queue (WP-H3, plan section 4.6).
	//
	// Review is a first-class workflow, not a background job, so the card
	// carries what a decision needs: the statement, its type and confidence,
	// the cited excerpts, who was in the meeting, and whether the source
	// contained instruction-shaped text.
	authed.HandleFunc("GET /v1/candidates", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if store == nil || id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		cards, err := store.PendingCandidates(r.Context(), id.UserID, limit)
		if err != nil {
			log.Error("reading the review queue", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, cards)
	})

	// What review DID.
	//
	// The queue answers "what needs deciding"; this answers the question that
	// comes straight after and had nowhere to be asked: a reviewer accepted a
	// task last week, so where is it. An accepted candidate with no bead shows
	// up as accepted with no bead, rather than as a decision that looks
	// complete.
	authed.HandleFunc("GET /v1/candidates/decided", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if store == nil || id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		cards, err := store.DecidedCandidates(r.Context(), id.UserID, limit)
		if err != nil {
			log.Error("reading decided candidates", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, cards)
	})

	// Deciding on one candidate.
	authed.HandleFunc("POST /v1/candidates/{id}/review", func(w http.ResponseWriter, r *http.Request) {
		ident, _ := authn.FromContext(r.Context())
		if store == nil || ident.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		var body struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
			Edited   string `json:"edited_statement"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed body"})
			return
		}
		result, err := store.Review(r.Context(), ident.UserID, r.PathValue("id"),
			body.Decision, body.Reason, body.Edited)
		switch {
		case errors.Is(err, domain.ErrNotPending):
			// 409, not 400. Two reviewers reaching the queue together is
			// expected; the second one being told is the difference between a
			// race and a silently lost decision.
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "this candidate has already been decided"})
			return
		case errors.Is(err, domain.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		case err != nil:
			// The domain errors here name what is missing rather than a
			// constraint, so they are safe and useful to return.
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	authed.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "not implemented",
			"path":  r.URL.Path,
		})
	})

	// The write surface over domain code that already exists (wg-p4h.4).
	registerWrites(authed, inbox, idem, log)
	registerProjectWrites(authed, store, idem, log)
	registerPlatformState(authed, db, log)
	registerBeadDetail(authed, db, log)

	// The event stream and cursor pagination (wg-p4h.8).
	registerStream(authed, eventLog, log)

	// The action check sits between authentication and the handler (wg-p4h.3).
	//
	// It runs AFTER routing, because it needs to know which route matched, and
	// http.ServeMux only knows that once it has matched. Handler(r) returns the
	// pattern without serving, which is how the check learns the route's name
	// before the handler runs rather than after it has already acted.
	//
	// What it does today is narrow TOKENS. A session passes through unchanged,
	// because deciding whether a PERSON holds a grant needs a role-to-action
	// mapping that does not exist yet — the roles table carries names and
	// descriptions and nothing connects them to authz's actions. Enabling an
	// owner check against an empty mapping would deny every request. Until it
	// lands a person's reach is bounded by row-level security exactly as it has
	// been, and a token is bounded by that AND its scopes.
	enforced := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := authed.Handler(r)
		id, _ := authn.FromContext(r.Context())

		if ok, reason := checkRouteAction(pattern, id); !ok {
			// The refusal names the CREDENTIAL rather than the person. An
			// operator debugging "this token may not" must not be sent to
			// change their role, which is the wrong fix and a confusing one.
			log.Info("route refused",
				"pattern", pattern, "reason", reason,
				"user", id.UserID, "token", id.TokenID)
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "this credential may not perform that action",
				"code":  reason,
			})
			return
		}
		// Per-token rate limit (wg-p4h.9).
		//
		// ADR-0022 gates spend per bead, which stops one bead running away and
		// does nothing about a client creating four hundred beads each within
		// budget. This is the missing ceiling, and it is per TOKEN so one
		// misconfigured tool throttles itself rather than its owner.
		//
		// Sessions are exempt: a person clicking a button is their own rate
		// limit, and keying them all on an empty token id would throttle every
		// browser user together.
		if id.ViaToken() && apiTokens != nil {
			perMinute, budget, err := apiTokens.Limits(r.Context(), id.TokenID)
			if err != nil {
				// Unknown limits are not permission to proceed unmetered, but
				// they are also not the caller's fault. Log and admit at the
				// default rather than refusing valid work over a read failure.
				log.Warn("reading token limits", "token", id.TokenID, "error", err)
				perMinute = 60
			}

			if ok, retry := limiter.Allow(id.TokenID, perMinute); !ok {
				secs := int(retry.Seconds()) + 1
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				log.Info("token rate limited",
					"token", id.TokenID, "user", id.UserID, "rate", perMinute)
				writeJSON(w, http.StatusTooManyRequests, map[string]any{
					"error":       "too many requests for this token",
					"code":        "rate_limited",
					"retry_after": secs,
				})
				return
			}

			// The spend cap applies only to what spends. Reading the inbox
			// costs nothing, and refusing it because a dispatch budget is
			// exhausted would make an exhausted token useless for the one thing
			// its owner most needs to do next: find out what happened.
			if budget.Exceeded && spendsMoney(pattern) {
				log.Info("token spend cap reached",
					"token", id.TokenID, "user", id.UserID,
					"cap_cents", budget.CapCents, "spent_cents", budget.SpentCents)
				writeJSON(w, http.StatusForbidden, map[string]any{
					"error":       "this token has reached its daily spend cap",
					"code":        "spend_cap_reached",
					"cap_cents":   budget.CapCents,
					"spent_cents": budget.SpentCents,
					"resets":      "midnight UTC",
				})
				return
			}
		}

		// The owner half: does the PERSON hold this action? ADR-0026 decided
		// the role-to-action matrix; 0040_role_permissions.sql stores it.
		//
		// SHADOW MODE by default. Until this matrix existed, a person's reach
		// was bounded only by row-level security, so nobody has ever needed a
		// role_grants row to use the interface. Enforcing before checking that
		// real users hold real grants would lock people out mid-task, and the
		// first to find out would be somebody trying to work. So the check runs
		// and reports, and WG_AUTHZ_ENFORCE=true turns reporting into refusing
		// once the logs are quiet.
		if grants != nil {
			if action := actionFor(pattern); action != "" && id.UserID != "" {
				err := grants.Authorize(r.Context(), authz.Request{
					UserID: id.UserID,
					Action: action,
				})
				if err != nil {
					if !cfg.AuthzEnforce {
						log.Warn("authorisation would refuse this request",
							"pattern", pattern, "action", action,
							"user", id.UserID, "token", id.TokenID,
							"note", "reporting only; set WG_AUTHZ_ENFORCE=true to refuse")
					} else {
						log.Info("authorisation refused",
							"pattern", pattern, "action", action, "user", id.UserID)
						writeJSON(w, http.StatusForbidden, map[string]any{
							"error": "your role does not grant that action",
							"code":  "role_grant_missing",
						})
						return
					}
				}
			}
		}

		authed.ServeHTTP(w, r)
	})

	mux.Handle("/v1/", authn.Middleware(auth, resolver, log)(enforced))

	// The remote MCP transport, on the same hostname and behind the same Access
	// application. See cmd/control-api/mcp.go for why no OAuth lives here.
	//
	// `enforced` is passed as the dispatcher, not `authed`: a tool call must
	// pay the same rate limit, spend cap and role check as the same request
	// over HTTPS, and those live in enforced.
	registerMCP(mux, cfg, enforced, auth, resolver, log)

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
	// The SECOND documented exception, and the last: Google Pub/Sub pushes
	// Workspace Events here and cannot complete an Access challenge either.
	//
	// Unlike the webhook, the bypass is not the boundary. Pub/Sub attaches an
	// OIDC token that Google signs and this verifies against Google's public
	// keys, the issuer and an audience bound to this endpoint (ADR-0026). The
	// node holds no secret that could forge a delivery.
	//
	// Registered only when an audience is configured. A receiver with an empty
	// audience would accept any Google-signed token, which is worse than having
	// no endpoint at all because it looks like verification.
	if cfg.PubSubPushAudience != "" {
		receiver := &workspace.Receiver{
			Validator: &workspace.PushValidator{
				Audience:       cfg.PubSubPushAudience,
				ServiceAccount: cfg.PubSubPushServiceAccount,
			},
			DB:  db,
			Log: log,
		}
		mux.Handle("POST /v1/google/events", receiver)
	} else {
		log.Warn("Pub/Sub push endpoint not registered: no audience configured")
	}

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
// nullableTime parses an RFC 3339 timestamp, or NULL when it is absent or
// unparseable.
//
// Unparseable rather than an error, because this reads a field a node sends and
// a bad timestamp on one bead's comment must not fail the whole projection
// pass. The comment text is the useful part; losing its clock is a nuisance,
// losing the pass is an outage.
func nullableTime(v string) any {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil
	}
	return t
}

func nullableParam(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}
