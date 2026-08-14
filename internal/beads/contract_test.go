package beads

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/datopian/workgraph/internal/domain"
)

// Contract tests run against the REAL pinned bd binary in a throwaway database.
//
// They exist because the adapter's contract is a CLI, not a library: argument
// order, JSON field names and exit codes are all things upstream can change
// without any compiler noticing. Plan section 7.6 requires exactly this before
// a version bump is accepted.
//
// They skip when bd is absent, so CI without the toolchain still runs.
func newTestDB(t *testing.T) (*CLIClient, DatabaseRef) {
	t.Helper()

	bd, err := exec.LookPath("bd")
	if err != nil {
		t.Skip("bd not on PATH; run make bootstrap and add .toolchain/bin")
	}

	dir := t.TempDir()
	// Beads discovers its database from the working directory, and bd init
	// wants a git repository.
	for _, args := range [][]string{
		{"git", "init", "-q", "-b", "main", "."},
		{bd, "init", "--prefix", "tc"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("could not prepare a test database (%v): %s", err, out)
		}
	}

	c := &CLIClient{
		Binary:         bd,
		OrganisationID: "org-test",
		Actor:          "contract-test",
		DatabasePaths:  map[string]string{"db-test": dir},
	}
	return c, DatabaseRef{ID: "db-test", CellID: "cell-test", Path: dir, Scope: domain.ScopeProject}
}

func TestContractCreateAndGet(t *testing.T) {
	c, db := newTestDB(t)
	ctx := context.Background()

	ref, err := c.Create(ctx, db, Issue{
		Title:       "A contract test bead",
		Description: "created by the adapter",
		Type:        "task",
		Priority:    1,
		Labels:      []string{"kind:task", "scope:project"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The full identity tuple, not just the bead ID — project prefixes collide
	// across databases (plan section 7.3).
	if ref.BeadID == "" || ref.OrganisationID != "org-test" || ref.BeadsDatabaseID != "db-test" {
		t.Fatalf("create returned an incomplete reference: %+v", ref)
	}

	got, err := c.Get(ctx, ref)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "A contract test bead" {
		t.Errorf("title round-trip failed: %q", got.Title)
	}
	if got.Ref.BeadID != ref.BeadID {
		t.Errorf("get returned a different bead: %s vs %s", got.Ref.BeadID, ref.BeadID)
	}
	// Labels carry the business semantics, so losing them silently would break
	// every scope and visibility rule built on top (plan section 7.2).
	if len(got.Labels) == 0 {
		t.Error("labels did not survive the round trip")
	}
}

func TestContractListAndReady(t *testing.T) {
	c, db := newTestDB(t)
	ctx := context.Background()

	for _, title := range []string{"first", "second"} {
		if _, err := c.Create(ctx, db, Issue{Title: title, Type: "task"}); err != nil {
			t.Fatal(err)
		}
	}

	all, err := c.List(ctx, db, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) < 2 {
		t.Fatalf("expected at least 2 issues, got %d", len(all))
	}

	ready, err := c.Ready(ctx, db)
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	if len(ready) == 0 {
		t.Error("two unblocked issues should both be ready")
	}
}

// The argument order of `bd dep add` decides which item is blocked. Getting it
// backwards silently inverts readiness, which is the kind of bug that looks
// like a scheduling mystery rather than a wiring error.
func TestContractDependencyDirection(t *testing.T) {
	c, db := newTestDB(t)
	ctx := context.Background()

	blocker, err := c.Create(ctx, db, Issue{Title: "the blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := c.Create(ctx, db, Issue{Title: "the blocked", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Link(ctx, blocked, blocker, "blocks"); err != nil {
		t.Fatalf("link: %v", err)
	}

	ready, err := c.Ready(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var readyIDs []string
	for _, i := range ready {
		readyIDs = append(readyIDs, i.Ref.BeadID)
	}
	joined := strings.Join(readyIDs, " ")

	if !strings.Contains(joined, blocker.BeadID) {
		t.Errorf("the blocker should still be ready; ready = %s", joined)
	}
	if strings.Contains(joined, blocked.BeadID) {
		t.Errorf("the blocked item must NOT be ready — the dependency is inverted; ready = %s", joined)
	}
}

func TestContractCloseRequiresEvidence(t *testing.T) {
	c, db := newTestDB(t)
	ctx := context.Background()

	ref, err := c.Create(ctx, db, Issue{Title: "to be closed", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Close(ctx, ref, ""); err == nil {
		t.Fatal("closing without evidence must be refused")
	}
	if err := c.Close(ctx, ref, "closed by the contract test with evidence"); err != nil {
		t.Fatalf("close with evidence: %v", err)
	}

	got, err := c.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" {
		t.Errorf("expected closed, got %q", got.Status)
	}
}

// Every invocation must be recorded: a write that is not recorded cannot be
// reconciled after a failure (plan section 7.1).
func TestContractInvocationsAreRecorded(t *testing.T) {
	c, db := newTestDB(t)
	var records []CommandRecord
	c.Record = func(r CommandRecord) { records = append(records, r) }

	if _, err := c.Create(context.Background(), db, Issue{Title: "recorded", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("no invocation was recorded")
	}
	r := records[len(records)-1]
	if r.Actor != "contract-test" || r.Database != "db-test" || r.Duration <= 0 {
		t.Errorf("incomplete record: %+v", r)
	}
}

func TestContractBackup(t *testing.T) {
	c, db := newTestDB(t)
	if err := c.Backup(context.Background(), db); err != nil {
		t.Fatalf("bd backup: %v", err)
	}
}

// The pinned version must be what versions.lock declares. Gas Town v1.2.0
// refuses a newer bd, so a drifted binary is a real incompatibility.
func TestContractPinnedVersion(t *testing.T) {
	c, db := newTestDB(t)
	v, err := c.Version(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.ReadFile(filepath.Join("..", "..", "versions.lock"))
	if err != nil {
		t.Skip("versions.lock unreadable")
	}
	if !strings.Contains(v, "1.0.4") {
		t.Errorf("bd reports %q but versions.lock pins v1.0.4", v)
	}
	if !strings.Contains(string(lock), `version: "v1.0.4"`) {
		t.Error("versions.lock no longer pins bd v1.0.4; the contract tests need revisiting")
	}
}
