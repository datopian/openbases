package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/datopian/workgraph/internal/work"
)

// Every call this daemon makes must be under /v1/node/.
//
// It shipped calling /v1/work instead — the endpoints a PERSON uses, behind the
// identity Access application. The token was right and the path was wrong, and
// the way that presents is not a 403: Access answers with 200 and its login
// page, so the client fails on `invalid character '<'` while parsing HTML as
// JSON, once every ten seconds, saying nothing about Access or about the path.
//
// The split is a security boundary, not a naming convention. A cell token may
// claim a job and report a result; it may not enqueue one. A client that drifts
// back to the human paths is asking for permissions it should not have, so this
// asserts the prefix rather than the three exact routes — a new endpoint added
// under the wrong prefix should fail here too.
func TestEveryCallUsesTheNodePrefix(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		// 204 for the claim, so the pass ends without trying to run an agent.
		if strings.HasSuffix(r.URL.Path, "/claim") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	d := &dispatcher{
		api:      srv.URL,
		cell:     "oss",
		rig:      "sandbox",
		deadline: time.Minute,
		http:     srv.Client(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx := context.Background()

	if _, err := d.claim(ctx); err != nil {
		t.Fatalf("claim: %v", err)
	}
	d.report(ctx, "a-job-id", work.Result{OK: true, Output: "done"})
	if err := d.sendBeads(ctx, []beadRow{{Bead: "wg-1", Title: "t", Kind: "task", Status: "open"}}); err != nil {
		t.Fatalf("sendBeads: %v", err)
	}

	if len(paths) == 0 {
		t.Fatal("no requests were made; the test proves nothing")
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "/v1/node/") {
			t.Errorf("called %q; every dispatcher call must be under /v1/node/, "+
				"which is the prefix the cell service token is authorised for", p)
		}
	}
}

// An unauthenticated request to Access comes back 200 with a login page, not a
// 4xx, so a client that only checks the status code reports a JSON parse error
// and buries the cause. The message has to name Access, or the next person
// spends the time again.
func TestAccessLoginPageIsReportedAsSuch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<!DOCTYPE html><html><body>Sign in</body></html>")
	}))
	defer srv.Close()

	d := &dispatcher{
		api:  srv.URL,
		cell: "oss",
		http: srv.Client(),
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	_, err := d.claim(context.Background())
	if err == nil {
		t.Fatal("a login page must be an error, not an empty claim")
	}
	if !strings.Contains(err.Error(), "Access") {
		t.Errorf("the error must name Cloudflare Access so the cause is findable; got %q", err)
	}
}

// The gateway token is not stored on its own anywhere. It lives inside the
// header string the agent sends, because the gateway only honours the
// credential when it arrives as a header — set it in the environment and the
// run reaches the gateway untagged, which looks like success and is not.
func TestGatewayTokenComesFromTheCellSettings(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d := &dispatcher{cellRoot: root}

	write(`{"env":{"ANTHROPIC_CUSTOM_HEADERS":"cf-aig-authorization: Bearer tok-abc123\nx-other: 1"}}`)
	got, err := d.gatewayToken()
	if err != nil {
		t.Fatalf("gatewayToken: %v", err)
	}
	if got != "tok-abc123" {
		t.Errorf("token = %q, want tok-abc123", got)
	}

	// Refused, not defaulted. A run without the gateway does not fail — it
	// succeeds straight against the provider, outside every budget.
	write(`{"env":{"ANTHROPIC_CUSTOM_HEADERS":"x-other: 1"}}`)
	if _, err := d.gatewayToken(); err == nil {
		t.Error("a missing token must refuse the run, not return an empty string")
	}

	if err := os.Remove(filepath.Join(root, ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.gatewayToken(); err == nil {
		t.Error("missing settings must refuse the run")
	}
}

// The town's rigs are read from disk, and the things beside them are not rigs.
//
// A rig is a directory whose config.json SAYS "type": "rig". The presence of
// the file is not enough: town/settings has one saying "town-settings", and
// treating it as a rig made the staging dispatcher log
// `rig=settings error="bd list: signal: killed"` on every pass -- bd there
// hangs rather than failing, so it cost a whole pass -- and registered
// `settings` as a rig holding no repository.
func TestOnlyDirectoriesThatSayTheyAreRigsCountAsRigs(t *testing.T) {
	root := t.TempDir()
	town := filepath.Join(root, "town")
	for _, rig := range []string{"sandbox", "portaljs", "autoclaw"} {
		if err := os.MkdirAll(filepath.Join(town, rig), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(town, rig, "config.json"),
			[]byte(`{"type":"rig"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Everything a real town has beside its rigs.
	for _, other := range []string{"logs", "events", "plugins", "deacon"} {
		if err := os.MkdirAll(filepath.Join(town, other), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(town, "rigs.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// And settings, which DOES have a config.json -- with a different type.
	// Treating the file's presence as the discriminator made the staging
	// dispatcher poll it every pass, where bd hung until the deadline killed
	// it, and registered `settings` as a rig holding no repository.
	if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "settings", "config.json"),
		[]byte(`{"type":"town-settings","version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A config.json that cannot be parsed is not a rig either: a directory
	// wrongly included is polled and registered as routable, while one wrongly
	// excluded is merely invisible.
	if err := os.MkdirAll(filepath.Join(town, "truncated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "truncated", "config.json"),
		[]byte(`{"type":"ri`), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &dispatcher{cellRoot: root, rig: "sandbox", log: quietLog()}
	got := d.rigs()
	slices.Sort(got)
	want := []string{"autoclaw", "portaljs", "sandbox"}
	if !slices.Equal(got, want) {
		t.Errorf("rigs() = %v, want %v", got, want)
	}
}

// A town that is not there yet still yields the configured rig.
//
// A cell whose town has not been built reported one rig before this change and
// must keep reporting one: returning nothing would make the dispatcher project
// no beads and register nothing, which reads as a healthy empty cell.
func TestATownlessCellStillReportsItsConfiguredRig(t *testing.T) {
	d := &dispatcher{cellRoot: t.TempDir(), rig: "sandbox", log: quietLog()}
	if got := d.rigs(); !slices.Equal(got, []string{"sandbox"}) {
		t.Errorf("rigs() = %v, want [sandbox]", got)
	}
}

// The configured rig is included even when the town has no directory for it,
// for the same reason: it is the rig the unit was told to serve.
func TestTheConfiguredRigIsAlwaysIncluded(t *testing.T) {
	root := t.TempDir()
	town := filepath.Join(root, "town")
	if err := os.MkdirAll(filepath.Join(town, "portaljs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "portaljs", "config.json"),
		[]byte(`{"type":"rig"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &dispatcher{cellRoot: root, rig: "sandbox", log: quietLog()}
	got := d.rigs()
	slices.Sort(got)
	if !slices.Equal(got, []string{"portaljs", "sandbox"}) {
		t.Errorf("rigs() = %v, want [portaljs sandbox]", got)
	}
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
