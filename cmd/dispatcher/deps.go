package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// A per-bead worktree starts empty, and for a Node project that means every
// run reinstalls the world before it can touch the task.
//
// The numbers from 2026-09-11, all on the msf rig: the beads that finished in
// five minutes were the ones that write documents. Every bead that touches
// portal/ ran 11, 38, 98 minutes and died -- one of them killed at exactly ten
// minutes while npm had written 719 MB into node_modules, another still
// unpacking typescript 35 minutes in. The agent never reached the work it was
// dispatched for, and the run was billed for the wait.
//
// Sharing the refinery's checkout is what per-bead worktrees exist to stop
// (two runs editing one tree strand each other's work), so the fix is not to
// go back. It is to stop treating a dependency tree as part of the work: it
// is derived from a lockfile, identical for every worktree of the same rig,
// and hardlinking it costs no disk and no time.
//
// depsCache is where a rig keeps its copy.
func depsCache(cellRoot, rig string) string {
	return cellRoot + "/town/" + rig + "/wg-deps"
}

// dependencyDirs are the places a Node project keeps a node_modules: the
// directories holding a package.json, at the top or one level down. Bounded
// deliberately -- walking a whole checkout looking for package.json finds the
// ones INSIDE node_modules, thousands of them.
func dependencyDirs(root string) []string {
	var found []string
	if _, err := os.Stat(filepath.Join(root, "package.json")); err == nil {
		found = append(found, ".")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return found
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "node_modules" {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "package.json")); err == nil {
			found = append(found, e.Name())
		}
	}
	return found
}

// warmDependencies hardlinks the rig's cached node_modules into a fresh
// worktree, so a run starts with its dependencies already present.
//
// Hardlinks, not copies: the cache and the worktree are on the same
// filesystem by construction (both under <cellRoot>/town/<rig>), so this
// costs no disk and finishes in seconds for 700 MB. A run that then edits a
// file inside node_modules would edit the cache too -- which is why the cache
// is refreshed from, and never treated as, a run's output.
func (d *dispatcher) warmDependencies(rig, worktree string) {
	cache := depsCache(d.cellRoot, rig)
	for _, rel := range dependencyDirs(worktree) {
		src := filepath.Join(cache, rel, "node_modules")
		dst := filepath.Join(worktree, rel, "node_modules")
		if _, err := os.Stat(src); err != nil {
			continue // nothing cached for this project yet
		}
		if _, err := os.Stat(dst); err == nil {
			continue // the worktree already has one
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			d.log.Warn("could not prepare a dependency directory", "dir", dst, "error", err)
			continue
		}
		// cp -al: archive, hardlinking regular files. GNU cp on the node;
		// the flags are the same on the BSD cp a developer might have.
		out, err := exec.Command("cp", "-al", src, dst).CombinedOutput()
		if err != nil {
			d.log.Warn("could not warm dependencies; the run will install its own",
				"rig", rig, "from", src, "error", err, "output", strings.TrimSpace(string(out)))
			continue
		}
		d.log.Info("dependencies warmed from the rig cache", "rig", rig, "dir", rel)
	}
}

// saveDependencies refreshes the rig's cache from a finished run.
//
// Called after the landing, so what is cached is what a run actually used.
// The previous cache is replaced rather than merged: a node_modules is a
// whole, and half of one is worse than none.
func (d *dispatcher) saveDependencies(rig, worktree string) {
	cache := depsCache(d.cellRoot, rig)
	for _, rel := range dependencyDirs(worktree) {
		src := filepath.Join(worktree, rel, "node_modules")
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(cache, rel, "node_modules")
		staging := dst + ".incoming"
		_ = os.RemoveAll(staging)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			d.log.Warn("could not prepare the dependency cache", "dir", dst, "error", err)
			continue
		}
		if out, err := exec.Command("cp", "-al", src, staging).CombinedOutput(); err != nil {
			d.log.Warn("could not stage the dependency cache", "rig", rig,
				"error", err, "output", strings.TrimSpace(string(out)))
			_ = os.RemoveAll(staging)
			continue
		}
		// Swapped in only once it is complete: a run that dies mid-copy must
		// not leave the next one warming from half a dependency tree.
		old := dst + ".old"
		_ = os.RemoveAll(old)
		if _, err := os.Stat(dst); err == nil {
			if err := os.Rename(dst, old); err != nil {
				d.log.Warn("could not rotate the dependency cache", "dir", dst, "error", err)
				_ = os.RemoveAll(staging)
				continue
			}
		}
		if err := os.Rename(staging, dst); err != nil {
			d.log.Warn("could not install the dependency cache", "dir", dst, "error", err)
			_ = os.RemoveAll(staging)
			continue
		}
		_ = os.RemoveAll(old)
		d.log.Info("dependency cache refreshed", "rig", rig, "dir", rel)
	}
}
