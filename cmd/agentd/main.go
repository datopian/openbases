// Command agentd is the execution-node daemon.
//
// It accepts only typed jobs over mTLS with a signed job capability. There is
// deliberately no generic run_shell endpoint: emergency shell access belongs to
// a separate audited administrative path (plan section 12.4).
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

	"github.com/datopian/workgraph/internal/version"
)

// action is a typed job the daemon accepts. The set is closed.
type action string

const (
	cellCreate         action = "cell.create"
	cellHealth         action = "cell.health"
	repositoryClone    action = "repository.clone"
	beadCreate         action = "bead.create"
	beadUpdate         action = "bead.update"
	beadDispatch       action = "bead.dispatch"
	agentPause         action = "agent.pause"
	agentResume        action = "agent.resume"
	agentTerminate     action = "agent.terminate"
	logsStream         action = "logs.stream"
	gitDiff            action = "git.diff"
	pullRequestPrepare action = "pull_request.prepare"
	backupRun          action = "backup.run"
	versionInspect     action = "version.inspect"
)

var accepted = map[action]struct{}{
	cellCreate: {}, cellHealth: {}, repositoryClone: {}, beadCreate: {},
	beadUpdate: {}, beadDispatch: {}, agentPause: {}, agentResume: {},
	agentTerminate: {}, logsStream: {}, gitDiff: {}, pullRequestPrepare: {},
	backupRun: {}, versionInspect: {},
}

type jobRequest struct {
	Action     action          `json:"action"`
	Capability string          `json:"capability"`
	Payload    json.RawMessage `json:"payload"`
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	addr := os.Getenv("WG_AGENTD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8443"
	}
	log.Info("starting agentd", "addr", addr, "build", version.String())

	srv := &http.Server{
		Addr:              addr,
		Handler:           routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// TLSConfig with ClientAuth: tls.RequireAndVerifyClientCert is
		// configured by WP-E1, which also provisions the internal CA.
	}

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
		log.Error("agentd failed", "error", err)
		os.Exit(1)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Info("stopped cleanly")
}

func routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("POST /internal/job", handleJob)
	return mux
}

func handleJob(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed job"})
		return
	}
	// An unrecognised action is rejected. This is what keeps the daemon from
	// growing an accidental general-purpose execution surface.
	if _, ok := accepted[req.Action]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":  "unknown action",
			"action": string(req.Action),
		})
		return
	}
	// mTLS peer verification and capability signature checking land in WP-E1.
	// Until then every job is refused, so an unfinished daemon cannot execute.
	if req.Capability == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "signed capability required"})
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error":  "job execution not implemented (WP-E1)",
		"action": string(req.Action),
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
