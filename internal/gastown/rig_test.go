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
		if err := AddRig(context.Background(), tc.gt, tc.dir, tc.url); err == nil {
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
	err := AddRig(context.Background(), "/bin/true", file, "https://example.invalid/r.git")
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
	err := AddRig(context.Background(), script, t.TempDir(), "https://example.invalid/r.git")
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

	if err := AddRig(context.Background(), script, town, "https://example.invalid/r.git"); err != nil {
		t.Fatalf("add: %v", err)
	}

	cwd, _ := os.ReadFile(dir + "/cwd")
	// The temp dir may be a symlink (/var vs /private/var on macOS), so compare
	// the base rather than the whole path.
	if filepath.Base(strings.TrimSpace(string(cwd))) != "town" {
		t.Errorf("gt ran in %q, not in the town", strings.TrimSpace(string(cwd)))
	}
	args, _ := os.ReadFile(dir + "/args")
	if got := strings.TrimSpace(string(args)); got != "rig add https://example.invalid/r.git" {
		t.Errorf("gt was called with %q", got)
	}
}
