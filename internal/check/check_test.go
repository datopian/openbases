package check

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestARepositoryWithNoCommandIsNotChecked(t *testing.T) {
	// Not an error: this is every repository until somebody opts it in, and
	// reporting it as a failure would make every unconfigured repository look
	// broken.
	for _, cmd := range []string{"", "   ", "\n"} {
		got, err := Run(context.Background(), t.TempDir(), cmd, time.Minute)
		if err != nil || got != nil {
			t.Errorf("Run(%q) = %+v, %v; want nil, nil", cmd, got, err)
		}
	}
}

func TestAPassingAndAFailingCheck(t *testing.T) {
	dir := t.TempDir()

	pass, err := Run(context.Background(), dir, "true", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !pass.OK || pass.TimedOut {
		t.Errorf("`true` reported %+v", pass)
	}
	if pass.Command != "true" {
		t.Errorf("the command was recorded as %q", pass.Command)
	}

	fail, err := Run(context.Background(), dir, "echo 'FAIL: hero.test.tsx' >&2; exit 1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if fail.OK {
		t.Error("a failing command reported OK")
	}
	// stderr is kept: a compiler and a test runner both put the useful part
	// there, and a check that only captured stdout would report a silent
	// failure.
	if !strings.Contains(fail.Output, "hero.test.tsx") {
		t.Errorf("stderr was not captured: %q", fail.Output)
	}
}

// A shell, because a check command is a command line. `npm ci && npm test` is
// the ordinary shape and splitting it on spaces would break it.
func TestACommandLineRunsAsOne(t *testing.T) {
	got, err := Run(context.Background(), t.TempDir(), "true && echo second", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !got.OK || !strings.Contains(got.Output, "second") {
		t.Errorf("a two-part command line gave %+v", got)
	}
}

// The credential helper is gone, so nothing the check starts can mint a git
// token or push. This is the one mitigation that closes the route the tool
// policy's `git push` denial exists to close, so it is asserted directly
// rather than through HOME -- the first version of this test relied on git
// picking up HOME/.gitconfig, which it did not do here, and the test SKIPPED.
// A skipped test is not a passing one, least of all this one.
func TestTheCheckCannotReachTheGitCredentialHelper(t *testing.T) {
	config := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(config,
		[]byte("[credential]\n\thelper = /srv/cells/oss/bin/git-credential-workgraph\n"+
			"\tuseHttpPath = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Exactly what the cell user's environment amounts to: a global git config
	// naming the helper.
	t.Setenv("GIT_CONFIG_GLOBAL", config)

	// The fixture is real, proven with git itself and NOT through Run -- if
	// this fails the test is broken rather than the code.
	probe := exec.Command("git", "config", "--global", "--get", "credential.helper")
	probe.Env = os.Environ()
	if out, err := probe.CombinedOutput(); err != nil ||
		!strings.Contains(string(out), "git-credential-workgraph") {
		t.Fatalf("the fixture does not work, so the assertion below proves nothing: %v: %s", err, out)
	}

	got, err := Run(context.Background(), t.TempDir(),
		"git config --global --get credential.helper; echo rc=$?", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Output, "git-credential-workgraph") {
		t.Errorf("the check can still reach the credential helper, so a script "+
			"it starts could mint a token and push: %q", got.Output)
	}

	// Stronger, and without touching the network: a helper that leaves a
	// marker when it runs, asserted never to have run.
	//
	// The first version of this reached for `git ls-remote` on
	// datopian/portaljs and failed -- because that repository is PUBLIC, so
	// reading it needs no credential at all. The assertion was wrong rather
	// than the code, and a unit test that depends on a remote repository's
	// visibility is one that will keep being wrong.
	marker := filepath.Join(t.TempDir(), "helper-ran")
	spy := filepath.Join(t.TempDir(), "git-credential-spy")
	if err := os.WriteFile(spy,
		[]byte("#!/bin/sh\ntouch "+marker+"\necho username=x\necho password=y\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config,
		[]byte("[credential]\n\thelper = "+spy+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// git asks a helper when it needs credentials for a URL, so this is a
	// request that would use one.
	if _, err := Run(context.Background(), t.TempDir(),
		"git init -q . && git credential fill <<'EOF'\nprotocol=https\nhost=github.com\n\nEOF",
		30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the credential helper ran inside a check, so a script could mint a token and push")
	}

	// And the same request WITH the global config honoured does run it, so the
	// assertion above is about Run and not about git refusing helpers here.
	direct := exec.Command("/bin/sh", "-c",
		"cd "+t.TempDir()+" && git init -q . && git credential fill <<'EOF'\nprotocol=https\nhost=github.com\n\nEOF")
	direct.Env = os.Environ()
	_ = direct.Run()
	if _, err := os.Stat(marker); err != nil {
		t.Skip("git does not consult a credential helper in this environment, so the check above proves nothing")
	}
}

// The cell's own secrets are not in the environment of repository-controlled
// code.
func TestTheCheckDoesNotCarryTheCellsCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-must-not-appear")
	t.Setenv("WG_ACCESS_CLIENT_SECRET", "secret-must-not-appear")
	t.Setenv("PATH", os.Getenv("PATH")) // and something that must survive

	got, err := Run(context.Background(), t.TempDir(), "env", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"sk-must-not-appear", "secret-must-not-appear"} {
		if strings.Contains(got.Output, leaked) {
			t.Errorf("%s reached the check's environment", leaked)
		}
	}
	if !strings.Contains(got.Output, "PATH=") {
		t.Error("PATH did not survive, so nothing would be runnable")
	}
}

// A hanging check is distinguished from a failing one: they need different
// responses from whoever reads the result.
func TestAHangingCheckIsReportedAsSuch(t *testing.T) {
	got, err := Run(context.Background(), t.TempDir(), "sleep 30", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got.OK {
		t.Error("a hanging check reported OK")
	}
	if !got.TimedOut {
		t.Error("a hanging check was not reported as having timed out")
	}
	if !strings.Contains(got.Feedback(), "did not finish in time") {
		t.Errorf("the feedback does not say it timed out:\n%s", got.Feedback())
	}
}

func TestACheckNeedsATimeout(t *testing.T) {
	if _, err := Run(context.Background(), t.TempDir(), "true", 0); err == nil {
		t.Error("a check with no timeout was allowed; a hanging build would hold the rig")
	}
}

// The TAIL is kept, because a compiler names the file and line at the end and a
// test runner puts its summary there. Keeping the head would keep the banner.
func TestLongOutputKeepsTheEnd(t *testing.T) {
	got, err := Run(context.Background(), t.TempDir(),
		"for i in $(seq 1 4000); do echo \"line $i of noise\"; done; echo 'FAIL hero.test.tsx:12'; exit 1",
		time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Output, "FAIL hero.test.tsx:12") {
		t.Error("the end of the output was dropped, which is where the failure is")
	}
	if len(got.Output) > MaxOutput+64 {
		t.Errorf("the output is %d bytes; this goes in a pull request body", len(got.Output))
	}
	if !strings.Contains(got.Output, "earlier output dropped") {
		t.Error("the truncation is silent, so a reader cannot tell output is missing")
	}
	// And it cuts on a line boundary rather than mid-token.
	if first := strings.SplitN(got.Output, "\n", 3)[1]; !strings.HasPrefix(first, "line ") {
		t.Errorf("the truncation cut mid-line: %q", first)
	}
}

// A passing check produces no feedback: there is nothing for an agent to fix,
// and a prompt that says "the check passed" invites it to keep going.
func TestAPassingCheckGivesNoFeedback(t *testing.T) {
	if f := (&Result{OK: true}).Feedback(); f != "" {
		t.Errorf("a passing check produced feedback:\n%s", f)
	}
	if f := (*Result)(nil).Feedback(); f != "" {
		t.Errorf("no check at all produced feedback:\n%s", f)
	}
}
