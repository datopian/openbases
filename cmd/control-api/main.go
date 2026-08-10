// Command control-api serves the Workgraph HTTP API and the web application.
//
// The origin is reached only through Cloudflare Tunnel; it never listens on a
// public interface (plan sections 6.1, 11.2).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/datopian/workgraph/internal/config"
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

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           routes(cfg),
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

func routes(cfg config.ControlAPI) http.Handler {
	mux := http.NewServeMux()

	// Liveness answers "is the process running"; it must not depend on
	// downstream services, or a database blip would trigger a restart loop.
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	// Readiness answers "can this instance serve traffic". Until the
	// dependency checks of WP-C1 and WP-C2 exist, it reports not-ready rather
	// than claiming a health it cannot verify.
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready",
			"reason": "dependency checks not implemented (WP-C1)",
		})
	})

	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"version": version.Version,
			"commit":  version.Commit,
			"built":   version.BuildDate,
			"env":     string(cfg.Environment),
		})
	})

	// Every other route is unimplemented and must say so explicitly rather
	// than returning an empty success that a caller could mistake for data.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "not implemented",
			"path":  r.URL.Path,
		})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
