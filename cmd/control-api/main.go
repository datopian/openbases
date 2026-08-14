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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/workgraph/internal/authn"
	"github.com/datopian/workgraph/internal/config"
	"github.com/datopian/workgraph/internal/domain"
	"github.com/datopian/workgraph/internal/version"
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
		}
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           routes(cfg, db, auth, domain.NewResolver(domain.NewStore(db)), log),
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
	if db != nil {
		store = domain.NewStore(db)
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

	authed.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "not implemented",
			"path":  r.URL.Path,
		})
	})

	mux.Handle("/v1/", authn.Middleware(auth, resolver, log)(authed))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
