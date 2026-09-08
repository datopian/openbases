package gastown

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The arguments that cannot be missing, because each produces a different kind
// of confusing failure further in: no binary is a configuration mistake, no
// town means gt reports something about settings, and no URL means gt clones
// nothing and succeeds.
func TestAddRigRefusesIncompleteArguments(t *testing.T) {
	town := t.TempDir()
	for _, tc := range []struct{ name, gt, dir, url string }{
		{"no binary", "", town, "https://example.invalid/r.git"},
		{"no town", "/bin/true", "", "https://example.invalid/r.git"},
		{"no url", "/bin/true", town, ""},
	} {
		if err := AddRig(context.Background(), tc.gt, tc.dir, "myrig", tc.url, "mr7"); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

// gt reads the town from the CURRENT DIRECTORY, not from a flag, so a run from
// anywhere else finds no town and says something unhelpful about configuration.
// Checking first turns that into a sentence naming the directory.
func TestAddRigRefusesATownThatIsNotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-town")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := AddRig(context.Background(), "/bin/true", file, "myrig", "https://example.invalid/r.git", "mr7")
	if err == nil {
		t.Fatal("a file was accepted as a town")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

// gt's own message is the useful part: "repository is empty (no commits)" is a
// different problem from "authentication failed", and both read better than an
// exit status. The end is kept because that is where the error is.
func TestAddRigReportsWhatGtSaid(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-gt")
	body := "#!/bin/sh\necho 'repository is empty (no commits)' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	err := AddRig(context.Background(), script, t.TempDir(), "myrig", "https://example.invalid/r.git", "mr7")
	if err == nil {
		t.Fatal("a failing gt reported success")
	}
	if !strings.Contains(err.Error(), "no commits") {
		t.Errorf("gt's own message was lost: %v", err)
	}
}

// The happy path runs gt from inside the town, because that is the one thing
// about gt's interface that is easy to get wrong and silent when wrong.
func TestAddRigRunsFromInsideTheTown(t *testing.T) {
	dir := t.TempDir()
	town := filepath.Join(dir, "town")
	if err := os.Mkdir(town, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "fake-gt")
	// Records the directory it was run in and the arguments it got.
	body := "#!/bin/sh\npwd > " + dir + "/cwd\necho \"$@\" > " + dir + "/args\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := AddRig(context.Background(), script, town, "myrig", "https://example.invalid/r.git", "mr7"); err != nil {
		t.Fatalf("add: %v", err)
	}

	cwd, _ := os.ReadFile(dir + "/cwd")
	// The temp dir may be a symlink (/var vs /private/var on macOS), so compare
	// the base rather than the whole path.
	if filepath.Base(strings.TrimSpace(string(cwd))) != "town" {
		t.Errorf("gt ran in %q, not in the town", strings.TrimSpace(string(cwd)))
	}
	args, _ := os.ReadFile(dir + "/args")
	if got := strings.TrimSpace(string(args)); got != "rig add myrig https://example.invalid/r.git --prefix mr7" {
		t.Errorf("gt was called with %q", got)
	}
}

// A missing prefix is refused rather than left to gt.
//
// gt derives one from the name, and it would not be the prefix
// system_rigs_wanted already recorded -- so the rig's beads would carry ids
// nothing else expects. A bead id is never rewritten, so this is the one
// argument where a helpful default is permanent damage.
func TestAddRigRefusesAMissingPrefix(t *testing.T) {
	err := AddRig(context.Background(), "/bin/true", t.TempDir(), "myrig",
		"https://example.invalid/r.git", "")
	if err == nil {
		t.Fatal("a missing prefix was accepted, so gt would have chosen the bead ids")
	}
	if !strings.Contains(err.Error(), "disagree") {
		t.Errorf("the error does not explain the consequence: %v", err)
	}
}

// gt puts its error FIRST and its entire usage text after it, so keeping the
// tail buries the message in --sparse-checkout documentation. That is not
// hypothetical: the first version of AddRig called gt without a rig name, and
// the failure that reached the log was forty lines of flags with the real
// complaint cut off the front.
func TestReasonDropsTheUsageBlock(t *testing.T) {
	out := "Error: accepts 2 arg(s), received 1\n" +
		"Usage:\n  gt rig add [name] [url]\n" +
		"      --prefix string   Beads issue prefix\n" +
		"      --sparse-checkout strings   Sparse checkout paths\n"
	got := reason(out)
	if !strings.Contains(got, "accepts 2 arg(s)") {
		t.Errorf("gt's actual error was lost: %q", got)
	}
	if strings.Contains(got, "sparse-checkout") {
		t.Errorf("the usage block survived and will bury the error: %q", got)
	}
}

// A git failure underneath gt comes LAST, after git's progress output, so
// keeping only the first line reports "Cloning into bare repository..." --
// which is not an error -- and hides the credential failure below it.
func TestReasonKeepsAnErrorBelowGitProgress(t *testing.T) {
	out := "Cloning into bare repository '/srv/cells/oss/town/x'...\n" +
		"git-credential-workgraph: no token issued for data-portal-examples\n" +
		"fatal: could not read Username for 'https://github.com'\n"
	got := reason(out)
	if !strings.Contains(got, "no token issued") {
		t.Errorf("the credential failure was lost: %q", got)
	}
}
