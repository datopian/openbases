package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/datopian/openbases/internal/work"
)

// An agent that is reading and thinking is not a stalled agent.
//
// This is msf8-17x on 2026-09-11, reproduced. With its dependencies already
// warm it was stopped after 10m30s for "nothing written (no output, no file
// changed)" -- while its own log showed it reading the repository and waiting
// on a ninety-second model call:
//
//	12:46:51 loop session.id=ses_f6f7ef25… step=5
//	12:46:51 stream model=workers-ai/@cf/zai-org/glm-5.3-flash
//	12:48:23 evaluated permission=read … portal/package.json
//
// Reading changes no file and opencode says almost nothing on stdout, so a
// working agent was indistinguishable from a wedged one.
func TestAnAgentReadingAndThinkingIsNotStalled(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	cellRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cellRoot, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cellRoot, ".claude", "settings.json"),
		[]byte(`{"env":{"ANTHROPIC_CUSTOM_HEADERS":"cf-aig-authorization: Bearer tok"}}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	checkout := filepath.Join(cellRoot, "town", "sandbox", "refinery", "rig")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(transcriptDir(cellRoot, "sa-read"), "opencode", "log")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Writes ONLY to its own log, the way an agent reading files does, and
	// says nothing on stdout.
	runner := filepath.Join(t.TempDir(), "runner.sh")
	script := "#!/bin/sh\n" +
		"i=0\n" +
		"while [ $i -lt 20 ]; do\n" +
		"  echo \"level=INFO message=evaluated permission=read step=$i\" >> " +
		filepath.Join(logDir, "opencode.log") + "\n" +
		"  i=$((i+1)); sleep 0.6\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(runner, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	d := &dispatcher{
		api: srv.URL, cell: "oss", rig: "sandbox", cellRoot: cellRoot,
		runner: runner, http: srv.Client(),
		deadline: time.Hour,
		stall:    5 * time.Second, // shorter than the run takes
		tick:     200 * time.Millisecond,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	out, err := d.run(context.Background(),
		work.Job{ID: "j1", Bead: "sa-read", Kind: "work", Cell: "oss", Rig: "sandbox"}, "")
	if err != nil {
		t.Fatalf("an agent that was reading and thinking was stopped: %v\n%s", err, out)
	}
	// And what it was doing is in the outcome, not only on the node.
	if !strings.Contains(out, "agent transcript") || !strings.Contains(out, "permission=read") {
		t.Errorf("the outcome does not carry the agent's transcript: %q", out)
	}
}

// The tail is the END of the log, because what a reader needs is what the
// agent was doing when it stopped.
func TestTheTranscriptTailIsTheEndOfTheLog(t *testing.T) {
	cellRoot := t.TempDir()
	logDir := filepath.Join(transcriptDir(cellRoot, "sa-1"), "opencode", "log")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := range 500 {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", 40))
		b.WriteString(" ")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString("\n")
	}
	b.WriteString("THE LAST THING IT DID\n")
	if err := os.WriteFile(filepath.Join(logDir, "opencode.log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	tail := transcriptTail(cellRoot, "sa-1", 1<<10)
	if !strings.Contains(tail, "THE LAST THING IT DID") {
		t.Error("the tail does not include the end of the log")
	}
	if len(tail) > 1<<10 {
		t.Errorf("the tail is %d bytes; a job result is not the place for a whole log", len(tail))
	}
	if strings.HasPrefix(tail, "ine ") || strings.HasPrefix(tail, "ne ") {
		t.Errorf("the tail begins mid-line: %q", tail[:20])
	}
}

// A run with no transcript reports its output unchanged, rather than an empty
// "transcript" heading that suggests something was captured.
func TestNoTranscriptAddsNothing(t *testing.T) {
	if got := withTranscript("the output", ""); got != "the output" {
		t.Errorf("withTranscript added something when there was no log: %q", got)
	}
	if got := transcriptTail(t.TempDir(), "sa-missing", 1024); got != "" {
		t.Errorf("a missing state directory produced %q", got)
	}
}
