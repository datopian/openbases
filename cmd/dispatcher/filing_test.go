package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/datopian/openbases/internal/work"
)

// A fake bd that records every call and keeps a tiny graph in a file.
//
// Filing is a SEQUENCE of bd calls whose order is the whole design -- create
// everything, then wire the edges -- and the order is what a test has to be
// able to see. Asserting it against the real bd would need a Dolt database on
// whatever machine runs the tests.
func fakeBD(t *testing.T) (bin, state string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "bd")
	state = filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "issues.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
S=` + state + `
# Log every invocation, one line each, in the order they happen.
echo "$@" >> $S/calls.log
shift 2   # -C <path>
case "$1 $2" in
"list --all")
  cat $S/issues.json
  exit 0 ;;
"dep add")
  exit 0 ;;
"dep cycles")
  # What bd actually prints when the graph is clean, ✓ and all. The first
  # version of the guard matched on this prose and read it as a cycle.
  cat $S/cycles 2>/dev/null || echo "[]"
  exit 0 ;;
esac
case "$1" in
create)
  n=$(cat $S/next 2>/dev/null || echo 1)
  echo $((n+1)) > $S/next
  id="sa-$n"
  ref=""
  while [ $# -gt 0 ]; do
    if [ "$1" = "--external-ref" ]; then ref="$2"; fi
    shift
  done
  # Append {"id":..,"external_ref":..} to the array.
  sed -i.bak "s|\]$|{\"id\":\"$id\",\"external_ref\":\"$ref\",\"title\":\"t\",\"status\":\"open\"}]|" $S/issues.json
  sed -i.bak 's|}{|},{|g' $S/issues.json
  echo "$id"
  exit 0 ;;
update)
  exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, state
}

func calls(t *testing.T, state string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, "calls.log"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func filingDispatcher(t *testing.T) (*dispatcher, string) {
	bin, state := fakeBD(t)
	return &dispatcher{
		cell: "oss", rig: "sandbox", cellRoot: t.TempDir(), bdBinary: bin,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, state
}

func planJob(t *testing.T, p work.Plan) work.Job {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return work.Job{ID: "j1", Kind: work.KindFile, Cell: "oss", Rig: "sandbox", Brief: string(b)}
}

// Every bead exists before any edge is drawn.
//
// A plan writes "deploy" above "scaffold" as often as below it, so wiring as
// you go fails on the first forward reference -- and fails HALF WAY, which is
// the worst outcome: the next re-plan has to reconcile a graph nobody can
// describe.
func TestEveryBeadIsFiledBeforeAnyDependencyIsWired(t *testing.T) {
	d, state := filingDispatcher(t)
	out, err := d.fileplan(context.Background(), planJob(t, work.Plan{
		Project: "msf",
		Beads: []work.PlanBead{
			// Deliberately in the awkward order: the blocked bead first.
			{Ref: "deploy", Title: "Deploy the portal", DependsOn: []string{"scaffold"}},
			{Ref: "scaffold", Title: "Scaffold the portal"},
		},
	}))
	if err != nil {
		t.Fatalf("filing a valid plan failed: %v", err)
	}

	log := calls(t, state)
	lastCreate, firstDep := -1, -1
	for i, c := range log {
		switch {
		case strings.Contains(c, " create "):
			lastCreate = i
		case strings.Contains(c, "dep add") && firstDep < 0:
			firstDep = i
		}
	}
	if lastCreate < 0 || firstDep < 0 {
		t.Fatalf("expected creates and a dep add, got: %v", log)
	}
	if firstDep < lastCreate {
		t.Errorf("a dependency was wired before every bead existed:\n%s",
			strings.Join(log, "\n"))
	}

	// And the edge points the right way: the BLOCKED bead is named first.
	// `bd dep add A B` means A is blocked by B, and getting this backwards
	// makes finished work block the work that needs it.
	var dep string
	for _, c := range log {
		if strings.Contains(c, "dep add") {
			dep = c
		}
	}
	// deploy was created first, so it is sa-1; scaffold is sa-2.
	if !strings.Contains(dep, "dep add sa-1 sa-2") {
		t.Errorf("the dependency is the wrong way round or names the wrong beads: %q", dep)
	}
	if !strings.Contains(dep, "--no-cycle-check") {
		t.Errorf("bulk wiring must skip the per-edge cycle check: %q", dep)
	}
	var got struct {
		Filed []work.Filed `json:"filed"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the result is not readable json: %v (%s)", err, out)
	}
	if len(got.Filed) != 2 || !got.Filed[0].Created {
		t.Errorf("the answer does not say what was filed: %s", out)
	}
}

// Re-filing the same plan revises the beads rather than duplicating them.
//
// The user re-plans often, and a plan that files a second copy of everything
// each time is not a plan tool, it is a bead generator.
func TestRefilingAPlanRevisesRatherThanDuplicates(t *testing.T) {
	d, state := filingDispatcher(t)
	job := planJob(t, work.Plan{
		Project: "msf",
		Beads:   []work.PlanBead{{Ref: "scaffold", Title: "Scaffold the portal"}},
	})
	if _, err := d.fileplan(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	out, err := d.fileplan(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}

	creates := 0
	updates := 0
	for _, c := range calls(t, state) {
		if strings.HasPrefix(c, "-C") && strings.Contains(c, " create ") {
			creates++
		}
		if strings.Contains(c, " update ") {
			updates++
		}
	}
	if creates != 1 {
		t.Errorf("re-filing created the bead again: %d creates", creates)
	}
	if updates != 1 {
		t.Errorf("re-filing did not revise the existing bead: %d updates", updates)
	}
	if !strings.Contains(out, `"created":false`) && strings.Contains(out, `"created":true`) {
		t.Errorf("the second filing reports the bead as new: %s", out)
	}
}

// A plan that leaves a cycle is reported loudly rather than silently filed.
//
// --no-cycle-check is what makes wiring a twelve-bead plan affordable; this is
// the other half of that bargain. A cycle blocks every bead in it for ever, by
// each other, and nothing in the interface says why.
func TestACycleLeftByAPlanIsReported(t *testing.T) {
	d, state := filingDispatcher(t)
	if err := os.WriteFile(filepath.Join(state, "cycles"),
		[]byte(`[["sa-1","sa-2","sa-1"]]`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := d.fileplan(context.Background(), planJob(t, work.Plan{
		Project: "msf",
		Beads: []work.PlanBead{
			{Ref: "a", Title: "A", DependsOn: []string{"b"}},
			{Ref: "b", Title: "B", DependsOn: []string{"a"}},
		},
	}))
	if err == nil {
		t.Fatal("a plan that left a cycle was reported as a clean filing")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("the error does not say a cycle is the problem: %v", err)
	}
	// The beads that WERE filed are still named: they exist, and a caller
	// told only "cycle" cannot tell whether anything was written.
	if !strings.Contains(out, "sa-1") {
		t.Errorf("the failure hides what was already filed: %s", out)
	}
}

// bd's own way of saying the graph is clean is not a cycle.
//
// This is the regression, and it cost a whole live filing: bd prints
//
//	✓ No dependency cycles detected
//
// and the guard looked for the substring "no cycles", which is not in it. Two
// beads were filed correctly, the dependency was wired correctly, and the job
// was marked failed. A person reading that would re-file, or worse, go
// looking for a cycle that never existed.
func TestBdSayingTheGraphIsCleanIsNotACycle(t *testing.T) {
	d, state := filingDispatcher(t)
	if err := os.WriteFile(filepath.Join(state, "cycles"),
		[]byte("✓ No dependency cycles detected"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := d.fileplan(context.Background(), planJob(t, work.Plan{
		Project: "msf",
		Beads:   []work.PlanBead{{Ref: "a", Title: "A"}},
	}))
	// Prose where JSON was asked for is a problem worth reporting -- but it
	// must not be reported as a CYCLE, which is a different and alarming
	// thing to tell somebody.
	if err != nil && strings.Contains(err.Error(), "cycle") &&
		!strings.Contains(err.Error(), "reading dep cycles") {
		t.Errorf("a clean graph was reported as a cycle: %v", err)
	}
}

// An invalid plan is refused before a single bead is written.
func TestAnInvalidPlanWritesNothing(t *testing.T) {
	d, state := filingDispatcher(t)
	_, err := d.fileplan(context.Background(), planJob(t, work.Plan{
		Project: "msf",
		Beads:   []work.PlanBead{{Ref: "a", Title: "A", DependsOn: []string{"nobody"}}},
	}))
	if err == nil {
		t.Fatal("a plan depending on a ref that does not exist was accepted")
	}
	if got := calls(t, state); len(got) != 0 {
		t.Errorf("a refused plan still ran bd: %v", got)
	}
}
