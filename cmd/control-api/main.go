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
	"github.com/datopian/workgraph/internal/config"
	"github.com/datopian/workgraph/internal/domain"
	"github.com/datopian/workgraph/internal/githubapp"
	"github.com/datopian/workgraph/internal/httplog"
	"github.com/datopian/workgraph/internal/version"
	"github.com/datopian/workgraph/internal/webui"
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
	var inbox *attention.Store
	var decisions *approvals.Store
	if db != nil {
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
		delivery, err := githubapp.VerifyWebhook(r, []byte(cfg.GitHubWebhookSecret))
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
