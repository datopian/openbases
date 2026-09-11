package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func depsDispatcher(t *testing.T) *dispatcher {
	t.Helper()
	return &dispatcher{
		cell: "oss", cellRoot: t.TempDir(),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A fresh worktree starts with its dependencies already there.
//
// This is the whole point: on 2026-09-11 every bead that touched the portal
// spent its run installing rather than working -- 11, 38 and 98 minutes, none
// of which reached the task -- while the beads that finished in five minutes
// were the ones that never ran npm.
func TestAFreshWorktreeStartsWithItsDependencies(t *testing.T) {
	d := depsDispatcher(t)
	cache := depsCache(d.cellRoot, "msf")
	write(t, filepath.Join(cache, "portal", "node_modules", "next", "index.js"), "dep")

	worktree := filepath.Join(d.cellRoot, "town", "msf", "wg-runs", "sa-1")
	write(t, filepath.Join(worktree, "portal", "package.json"), `{"name":"portal"}`)

	d.warmDependencies("msf", worktree)

	got := filepath.Join(worktree, "portal", "node_modules", "next", "index.js")
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("the worktree has no dependencies: %v", err)
	}
}

// And it costs no disk, because the files are hardlinks rather than copies.
//
// 700 MB per run would fill this node in a day: two leftover worktrees were
// already holding 1.4 GB before this existed.
func TestWarmingCostsNoDisk(t *testing.T) {
	d := depsDispatcher(t)
	cache := depsCache(d.cellRoot, "msf")
	src := filepath.Join(cache, "portal", "node_modules", "next", "index.js")
	write(t, src, "dep")

	worktree := filepath.Join(d.cellRoot, "town", "msf", "wg-runs", "sa-1")
	write(t, filepath.Join(worktree, "portal", "package.json"), `{}`)
	d.warmDependencies("msf", worktree)

	dst := filepath.Join(worktree, "portal", "node_modules", "next", "index.js")
	var a, b syscall.Stat_t
	if err := syscall.Stat(src, &a); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Stat(dst, &b); err != nil {
		t.Fatal(err)
	}
	if a.Ino != b.Ino {
		t.Errorf("the dependency was copied, not hardlinked (inodes %d vs %d): "+
			"700 MB a run fills the node", a.Ino, b.Ino)
	}
}

// A run's own install becomes the next run's warm start.
func TestAFinishedRunRefreshesTheCache(t *testing.T) {
	d := depsDispatcher(t)
	worktree := filepath.Join(d.cellRoot, "town", "msf", "wg-runs", "sa-1")
	write(t, filepath.Join(worktree, "portal", "package.json"), `{}`)
	write(t, filepath.Join(worktree, "portal", "node_modules", "next", "index.js"), "dep")

	d.saveDependencies("msf", worktree)

	cached := filepath.Join(depsCache(d.cellRoot, "msf"), "portal", "node_modules", "next", "index.js")
	if _, err := os.Stat(cached); err != nil {
		t.Fatalf("the cache was not refreshed from the run: %v", err)
	}
	// And no staging or rotation debris is left behind.
	for _, junk := range []string{".incoming", ".old"} {
		leftover := filepath.Join(depsCache(d.cellRoot, "msf"), "portal", "node_modules"+junk)
		if _, err := os.Stat(leftover); err == nil {
			t.Errorf("%s was left behind; a second copy of 700 MB is not free", leftover)
		}
	}
}

// An existing node_modules in the worktree is never overwritten: whatever the
// run installed for itself wins over the cache.
func TestWarmingNeverOverwritesWhatTheRunInstalled(t *testing.T) {
	d := depsDispatcher(t)
	write(t, filepath.Join(depsCache(d.cellRoot, "msf"), "portal", "node_modules", "marker"), "from cache")

	worktree := filepath.Join(d.cellRoot, "town", "msf", "wg-runs", "sa-1")
	write(t, filepath.Join(worktree, "portal", "package.json"), `{}`)
	write(t, filepath.Join(worktree, "portal", "node_modules", "marker"), "from the run")

	d.warmDependencies("msf", worktree)

	got, err := os.ReadFile(filepath.Join(worktree, "portal", "node_modules", "marker"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from the run" {
		t.Errorf("the cache overwrote the run's own dependencies: %q", got)
	}
	// And nothing was nested inside it. `cp -al src dst` with dst already a
	// directory does not fail -- it copies INTO it, leaving
	// node_modules/node_modules, which npm then walks for ever.
	nested := filepath.Join(worktree, "portal", "node_modules", "node_modules")
	if _, err := os.Stat(nested); err == nil {
		t.Errorf("warming nested a dependency tree inside itself at %s", nested)
	}
}

// package.json files INSIDE node_modules are not projects.
//
// A naive walk finds thousands of them and would try to cache a dependency of
// a dependency as though it were the repository.
func TestDependenciesOfDependenciesAreNotProjects(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "package.json"), `{}`)
	write(t, filepath.Join(root, "portal", "package.json"), `{}`)
	// npm writes a package.json at the root of node_modules in some setups,
	// and workspaces put one in every package. Neither is a project.
	write(t, filepath.Join(root, "node_modules", "package.json"), `{}`)
	write(t, filepath.Join(root, "node_modules", "next", "package.json"), `{}`)
	write(t, filepath.Join(root, "portal", "node_modules", "react", "package.json"), `{}`)

	got := dependencyDirs(root)
	if len(got) != 2 {
		t.Errorf("dependencyDirs(%q) = %v, want just the repository root and portal", root, got)
	}
}
