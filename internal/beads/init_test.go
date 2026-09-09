package beads

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The prefix is refused rather than defaulted, because it appears in every bead
// id in the graph and a bead id is never rewritten. This is the one argument
// where a helpful default is permanent damage.
func TestInitGraphRefusesAMissingPrefix(t *testing.T) {
	err := InitGraph(context.Background(), "/bin/true", "/bin/true", t.TempDir(), "")
	if err == nil {
		t.Fatal("a missing prefix was accepted")
	}
	if !strings.Contains(err.Error(), "cannot be changed later") {
		t.Errorf("the error does not say why it matters: %v", err)
	}
}

func TestInitGraphRefusesMissingBinaries(t *testing.T) {
	dir := t.TempDir()
	if err := InitGraph(context.Background(), "", "/bin/true", dir, "ab1"); err == nil {
		t.Error("a missing bd binary was accepted")
	}
	// dolt is not optional: bd shells out to it and segfaults without a usable
	// config, which is a crash rather than an error message.
	if err := InitGraph(context.Background(), "/bin/true", "", dir, "ab1"); err == nil {
		t.Error("a missing dolt binary was accepted")
	}
}

// An initialised graph is left alone. Re-running must not touch a graph that
// holds work: bd init on a live graph is not a no-op.
func TestInitGraphIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	// A binary that would fail if it ran at all, so a pass proves nothing ran.
	if err := InitGraph(context.Background(), "/bin/false", "/bin/false", dir, "ab1"); err != nil {
		t.Fatalf("an initialised graph was re-initialised: %v", err)
	}
}

// The guard is the config FILE, not the .beads directory.
//
// A failed init leaves the directory behind, so a directory-based check reports
// a segfaulted init as a finished one -- which is how a broken graph came to
// look healthy. Here the directory exists with no config, so init must proceed
// and the fake binary's failure must surface.
func TestInitGraphTreatsAPartialGraphAsUninitialised(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o750); err != nil {
		t.Fatal(err)
	}
	err := InitGraph(context.Background(), "/bin/false", "/bin/false", dir, "ab1")
	if err == nil {
		t.Fatal("a .beads directory with no config.yaml was treated as an initialised graph")
	}
	// And the partial directory is gone, because bd init will not repair one.
	if _, statErr := os.Stat(filepath.Join(dir, ".beads")); statErr == nil {
		t.Error("the partial .beads directory was left in place")
	}
}
