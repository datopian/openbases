package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/datopian/openbases/internal/landing"
	"github.com/datopian/openbases/internal/work"
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

// The base branch is the repository's own, never assumed to be main.
//
// Measured on the oss cell: `git symbolic-ref refs/remotes/origin/HEAD` fails
// in all twelve rigs, because `gt rig add` clones without recording a remote
// HEAD -- so git alone skips landing everywhere. And ckan/ckan's default branch
// is `master`, so a hardcoded "main" opens a pull request against a branch that
// does not exist, after the work has already been pushed.
func TestTheBaseBranchComesFromTheRepositoryNotFromAGuess(t *testing.T) {
	root := t.TempDir()
	write := func(rig, body string) {
		if err := os.MkdirAll(filepath.Join(root, "town", rig), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "town", rig, "config.json"),
			[]byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ckan", `{"type":"rig","name":"ckan","default_branch":"master"}`)
	write("portaljs", `{"type":"rig","name":"portaljs","default_branch":"main"}`)
	write("nameless", `{"type":"rig","name":"nameless"}`)

	d := &dispatcher{cellRoot: root, log: quietLog()}

	// git has no answer, which is the real state of every rig on the cell.
	silent := func(args ...string) (string, error) { return "", errors.New("no remote HEAD") }
	if got := d.defaultBranch(silent, "ckan"); got != "master" {
		t.Errorf("ckan's base is %q, want master; a pull request onto main would be refused", got)
	}
	if got := d.defaultBranch(silent, "portaljs"); got != "main" {
		t.Errorf("portaljs's base is %q, want main", got)
	}
	// Nothing knows: refuse rather than guess. Landing is skipped, and the
	// change stays in the tree where it can still be recovered.
	if got := d.defaultBranch(silent, "nameless"); got != "" {
		t.Errorf("a rig with no recorded branch answered %q, want a refusal", got)
	}

	// git's live answer wins over gt's record, which is what goes stale.
	live := func(args ...string) (string, error) { return "origin/develop\n", nil }
	if got := d.defaultBranch(live, "ckan"); got != "develop" {
		t.Errorf("git said develop and the answer was %q", got)
	}
}

// The agent's own words are pulled out of the runner's output, and a format
// that does not match yields nothing rather than a guess. A pull request body
// containing the runner's log lines would be worse than an empty one.
func TestTheAgentsSummaryIsSeparatedFromTheRunnerLog(t *testing.T) {
	out := `time=2026-09-07T05:27:26.487Z level=INFO msg=starting bead=sa-kfh
time=2026-09-07T05:28:10.231Z level=INFO msg="run completed" bead=sa-kfh duration=44s
sa-kfh: completed in 44s
Renamed the visible hero tab label from "Visual builder" to "Studio".
Left the two internal code comments untouched.`

	got := agentSummary("sa-kfh", out)
	if !strings.HasPrefix(got, `Renamed the visible hero tab label`) {
		t.Errorf("the summary starts wrongly:\n%s", got)
	}
	if strings.Contains(got, "level=INFO") {
		t.Errorf("the summary carries runner log lines:\n%s", got)
	}
	if !strings.Contains(got, "internal code comments") {
		t.Errorf("the summary is truncated:\n%s", got)
	}

	if s := agentSummary("sa-kfh", "no marker here at all"); s != "" {
		t.Errorf("an unrecognised format produced %q, want nothing", s)
	}
	// Another bead's marker is not this bead's summary.
	if s := agentSummary("sa-kfh", "sa-4yn: completed in 1s\nsomething else"); s != "" {
		t.Errorf("another bead's output was read as this one's: %q", s)
	}
}

// A run that is working says so, whichever harness it runs under.
//
// The heartbeat's other half reports stdout, and for opencode -- what polecat
// actually runs -- stdout arrives at exit. sa-7dc's first run therefore read
// "186 B of output, last wrote 7m0s ago" for its entire length while it was
// installing dependencies and writing a portal, which is indistinguishable
// from wedged. An agent's job is to change files, so the newest file it
// changed is the signal that does not depend on buffering.
func TestALiveRunIsVisibleWhateverTheHarnessBuffers(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// An old file, then the one the "agent" just wrote.
	write("README.md")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "README.md"), old, old); err != nil {
		t.Fatal(err)
	}
	write("portal/pages/index.tsx")

	got := touched(dir)
	if !strings.Contains(got, "portal/pages/index.tsx") {
		t.Errorf("touched(%q) = %q, want the file the run just wrote", dir, got)
	}

	// .git is skipped. git writes to it throughout a run, so counting it would
	// report every run as busy -- the same uselessness as the stdout signal,
	// in the opposite direction.
	write(".git/index")
	if got := touched(dir); strings.Contains(got, ".git") {
		t.Errorf("touched reported git's own bookkeeping: %q", got)
	}

	// And so is a dependency tree, which `npm install` fills with tens of
	// thousands of files that are not the run's work and would cost more to
	// walk than the run costs to make.
	write("node_modules/next/index.js")
	if got := touched(dir); strings.Contains(got, "node_modules") {
		t.Errorf("touched reported installed dependencies: %q", got)
	}

	// Nothing to report is reported as nothing, rather than as a reassuring
	// guess: a caller appends "" instead of "wrote something 0s ago".
	if got := touched(filepath.Join(dir, "does-not-exist")); got != "" {
		t.Errorf("touched on a missing checkout = %q, want empty", got)
	}
	if got := touched(""); got != "" {
		t.Errorf("touched on no checkout = %q, want empty", got)
	}
}

// A terminal transcript is presented as a log, not as the agent's words.
//
// datopian/msf#1 carried this verbatim under a bare `---`: escape sequences,
// command echoes and pages of directory listings. agentSummary returns
// everything wg-runner printed after its status line, which is prose for the
// claude harness and the whole raw session for opencode -- and opencode is
// what every polecat runs.
func TestATranscriptIsFoldedAwayAndProseIsNot(t *testing.T) {
	// Copied from the pull request, escape sequences included.
	transcript := "\x1b[0m\n> build · workers-ai/@cf/zai-org/glm-5.3-flash\n\x1b[0m" +
		"\x1b[0m$ \x1b[0mls -la /srv/cells/oss/runs/sa-7dc\ntotal 8\n" +
		"drwx------  2 wgcell_oss wgcell_oss 4096 Sep  9 09:49 .\n" +
		"-rw-r--r--  1 wgcell_oss wgcell_oss  227 Sep  8 20:01 README.md\n"

	got := tail(transcript)
	if strings.Contains(got, "\x1b") {
		t.Errorf("escape sequences reached the pull request body: %q", got)
	}
	if !strings.Contains(got, "<details><summary>Run log") {
		t.Errorf("a transcript was not folded away:\n%s", got)
	}
	if !strings.Contains(got, "```") {
		t.Errorf("a transcript was not fenced, so its output can be read as markdown:\n%s", got)
	}

	// Prose is shown plainly. An agent that wrote a real summary should not
	// have it hidden behind a disclosure triangle.
	prose := "I added the DCAT generator and wired it into the build.\n" +
		"The round-trip test passes."
	got = tail(prose)
	if strings.Contains(got, "<details>") {
		t.Errorf("the agent's own summary was folded away:\n%s", got)
	}
	if !strings.Contains(got, "DCAT generator") {
		t.Errorf("the agent's summary was lost:\n%s", got)
	}

	// Nothing is nothing, rather than an empty log block.
	if got := tail("   \n  \x1b[0m \n"); got != "" {
		t.Errorf("an empty summary produced %q", got)
	}

	// And a long transcript is trimmed to its end, where a failure happens.
	var many []string
	for i := 0; i < 200; i++ {
		many = append(many, fmt.Sprintf("$ step %d", i))
	}
	got = tail(strings.Join(many, "\n"))
	if !strings.Contains(got, "step 199") {
		t.Errorf("the end of the log was trimmed away, which is where a failure is:\n%s", got)
	}
	if strings.Contains(got, "step 0\n") {
		t.Errorf("the whole log was included; it should be capped:\n%s", got)
	}
	if !strings.Contains(got, "of 200 lines") {
		t.Errorf("the body does not say the log was trimmed:\n%s", got)
	}
}

// The body that is actually POSTed carries no escape sequences.
//
// Written because the first version of this test called tail() directly and so
// proved nothing about the pull request: reverting openPullRequest to append
// the summary raw -- the exact bug -- left it passing. A check on a helper the
// caller might not use is not a check.
func TestThePostedPullRequestBodyIsReadable(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		body = payload.Body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"url":"https://github.com/x/y/pull/1"}`)
	}))
	defer srv.Close()

	d := &dispatcher{
		api:  srv.URL,
		cell: "oss",
		http: srv.Client(),
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	transcript := "\x1b[0m$ \x1b[0mls -la /srv/cells/oss\ntotal 8\ndrwx------ 2 x x 4096 .\n"
	_, err := d.openPullRequest(context.Background(),
		work.Job{ID: "j1", Bead: "sa-7dc"}, "msf",
		&landing.Result{Branch: "bead/sa-7dc", Commit: "abc", Files: []string{"README.md"}},
		"main", transcript, "Scaffold a portal", nil,
		errors.New("stalled: nothing written for 10m1s"))
	if err != nil {
		t.Fatal(err)
	}
	if body == "" {
		t.Fatal("no body was posted; the test proves nothing")
	}
	if strings.Contains(body, "\x1b") {
		t.Errorf("escape sequences reached the posted body:\n%q", body)
	}
	if !strings.Contains(body, "<details><summary>Run log") {
		t.Errorf("the transcript was not folded away in the posted body:\n%s", body)
	}
	// The reason the run ended reaches the body, from the error the caller
	// passed. Asserted on the POSTED body rather than on a helper, because
	// that distinction is what let the raw-transcript bug survive its first
	// test: a check on a helper the caller might not use is not a check.
	if !strings.Contains(body, "Why it ended:") {
		t.Errorf("a failed run's body does not say why it ended:\n%s", body)
	}
	if !strings.Contains(body, "nothing written for 10m1s") {
		t.Errorf("the body does not carry the stall reason:\n%s", body)
	}

	// The things a reviewer needs are still there and still plain.
	for _, want := range []string{"sa-7dc", "The run FAILED", "README.md"} {
		if !strings.Contains(body, want) {
			t.Errorf("the posted body does not mention %q:\n%s", want, body)
		}
	}
}

// A run that is producing is left alone; one that has gone quiet is stopped.
//
// Elapsed time was the wrong measurement and this is the test that says so.
// Three of sa-7dc's four runs were killed mid-work by the old deadline -- one
// had installed dependencies and written a whole portal -- while the run that
// deserved stopping wrote no file for thirty minutes and was allowed its full
// allowance, because elapsed time cannot tell those two apart.
func TestARunIsStoppedWhenItStopsProducingNotWhenTheClockRunsOut(t *testing.T) {
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

	// A fake runner that writes to the checkout for a while, then goes silent
	// without exiting -- the shape of a wedged agent.
	runner := filepath.Join(t.TempDir(), "runner.sh")

	// The checkout the dispatcher will actually watch.
	//
	// run() sets job.Checkout itself, from the cell root and the rig -- a
	// caller cannot choose it. Passing one in the Job looked like it worked
	// and was silently discarded, so the progress watcher was pointed at a
	// directory that did not exist, saw no file change ever, and stopped the
	// run while it was writing. This test failing that way is what found it.
	checkout := filepath.Join(cellRoot, "town", "sandbox", "refinery", "rig")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	// Writes for about three seconds, then goes quiet WITHOUT exiting.
	//
	// `sleep` is deliberately not exec'd, so a grandchild holds the output
	// pipe open after the shell dies. That is the shape of a real run --
	// wg-runner starts opencode -- and it is what makes SIGKILL insufficient:
	// Wait blocks on the pipe until the grandchild finishes. An earlier
	// version of this test used `exec sleep` to make the hang go away, which
	// weakened the test to match the code instead of the other way round.
	script := "#!/bin/sh\n" +
		"for i in 1 2 3 4 5 6 7 8 9 10; do echo \"working $i\" > " + checkout + "/file-$i.txt; sleep 0.3; done\n" +
		"sleep 600\n"
	if err := os.WriteFile(runner, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	d := &dispatcher{
		api: srv.URL, cell: "oss", rig: "sandbox", cellRoot: cellRoot,
		runner: runner, http: srv.Client(),
		deadline: time.Hour, // the ceiling must not be what ends this
		stall:    2 * time.Second,
		// Often enough to observe the run while it is still writing files,
		// which is the half of this that must NOT trigger a stop.
		tick: 200 * time.Millisecond,
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	started := time.Now()
	_, err := d.run(context.Background(),
		work.Job{ID: "j1", Bead: "sa-7dc", Kind: "work", Cell: "oss", Rig: "sandbox"},
		"")
	took := time.Since(started)

	if err == nil {
		t.Error("a stalled run must report an error, not success: a run that produced " +
			"nothing and reported ok is how a wedged agent looks finished")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Errorf("the error does not say why the run ended: %v", err)
	}

	// It must have survived the productive stretch. The script writes for
	// ~2.4s, and the heartbeat ticks every 30s, so the first tick lands after
	// the writing is done -- what this asserts is that the run was not killed
	// before it finished producing, and was killed reasonably soon after.
	// It survived the productive stretch. The script writes for ~3s and the
	// loop looks every 200ms, so ticks land WHILE files are appearing: a run
	// stopped before 3s was stopped while it was working, which is the bug
	// this whole change is about. Only file changes signal progress here --
	// the script writes to files, never to stdout -- so this is also what
	// proves file changes count.
	if took < 3*time.Second {
		t.Errorf("the run was stopped after %s, while it was still writing files", took)
	}
	// And it was stopped promptly afterwards, rather than hanging. Without
	// SIGTERM and WaitDelay, Wait blocks on the pipe the orphaned grandchild
	// still holds and this run lasts as long as its sleep.
	if took > 60*time.Second {
		t.Errorf("the run took %s: it was not stopped promptly, which means the "+
			"stop left an orphan holding the output pipe", took)
	}

	// And the work it did produce is still on disk for the landing to find.
	// A stall stop must not be a rollback.
	if _, err := os.Stat(filepath.Join(checkout, "file-6.txt")); err != nil {
		t.Errorf("the stalled run's work was lost: %v", err)
	}
}

// A reviewer is told the shape of the change and why the run ended.
//
// Both halves were missing from datopian/msf#1 after the transcript was folded
// away. The file list was 77 backticked paths in one paragraph -- accurate and
// the least readable thing left in the body -- and the failure was reported as
// the bare words "The run FAILED", with no reason and, because a signalled
// wg-runner prints no status line, no log either.
func TestTheBodySaysTheShapeOfTheChangeAndWhyTheRunEnded(t *testing.T) {
	// Small changes are still listed in full: for three files a summary is
	// worse than the thing it summarises.
	small := fileSummary([]string{"README.md", "portal/package.json"})
	if !strings.Contains(small, "`README.md`") || strings.Contains(small, "<details>") {
		t.Errorf("a small change was summarised instead of listed: %s", small)
	}

	// A big one is grouped, counted, and its full list folded away.
	var many []string
	for i := 0; i < 74; i++ {
		many = append(many, fmt.Sprintf("portal/components/C%d.tsx", i))
	}
	many = append(many, "README.md", "ARCHITECTURE.md", "docs/adr/0001-x.md")

	got := fileSummary(many)
	if !strings.Contains(got, "**77 files changed:**") {
		t.Errorf("the count is not stated: %s", got)
	}
	if !strings.Contains(got, "`portal/` (74)") {
		t.Errorf("the tree that took the change is not named with its size: %s", got)
	}
	// The single files are named rather than counted, because "(1)" tells a
	// reviewer less than the name does.
	if !strings.Contains(got, "`README.md`") || !strings.Contains(got, "`ARCHITECTURE.md`") {
		t.Errorf("single files were not named: %s", got)
	}
	if !strings.Contains(got, "<details><summary>every path</summary>") {
		t.Errorf("the full list is not available at all: %s", got)
	}
	// Not 77 paths in the prose.
	head := strings.SplitN(got, "<details>", 2)[0]
	if strings.Count(head, "portal/components/") > 1 {
		t.Errorf("individual paths leaked into the summary line: %s", head)
	}
	if got := fileSummary(nil); !strings.Contains(got, "No files changed") {
		t.Errorf("an empty change reads as %q", got)
	}
}
