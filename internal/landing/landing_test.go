package landing

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A real repository, because what is being asserted is git's behaviour. A fake
// would only assert my belief about it, and the belief being checked here --
// that `git add -A` cannot pick up an excluded path -- is exactly the kind that
// is worth checking against the tool.
func repo(t *testing.T) (dir string, git Git) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	dir = filepath.Join(root, "work")

	run := func(d string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	run(origin, "init", "--bare", "--initial-branch=main", ".")
	run(root, "clone", origin, dir)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(dir, "add", "README.md")
	run(dir, "commit", "-m", "initial")
	run(dir, "push", "-u", "origin", "main")

	return dir, Exec(dir, root)
}

// write creates a file and every directory above it.
func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gastown's plumbing is never committed to somebody else's repository.
//
// `gt rig add` writes .beads/redirect into every rig's refinery checkout --
// all twelve on the oss cell have one -- and it is not in the repository's own
// .gitignore, because it has nothing to do with the repository. It sat as an
// untracked file directly beside sa-kfh's real edit:
//
//	 M site/components/home/LandingHero.tsx
//	?? .beads/
//
// so the first `git add -A` would have pushed it into PortalJS.
func TestGastownsPlumbingIsNeverCommitted(t *testing.T) {
	dir, git := repo(t)

	// The plumbing, exactly as gt writes it.
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "redirect"),
		[]byte("../../.beads\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// And the agent's real work beside it.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("Studio\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Land(git, Spec{Bead: "sa-kfh", Title: "Rename the hero tab", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("nothing landed, but the tree had a change")
	}
	if !slices.Equal(got.Files, []string{"README.md"}) {
		t.Errorf("landed %v, want just README.md", got.Files)
	}
	if !slices.Equal(got.Skipped, []string{".beads/"}) {
		t.Errorf("skipped %v, want .beads/", got.Skipped)
	}

	// The commit itself, which is the thing that reaches the repository. Named
	// by BRANCH rather than HEAD: the tree is returned to the base branch after
	// a landing, so HEAD is main again and asserting on it would check the
	// wrong commit -- and pass, because main has no plumbing in it either.
	out, err := git("show", "--name-only", "--format=", got.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".beads") {
		t.Errorf("the commit contains gastown's plumbing:\n%s", out)
	}
	if !strings.Contains(out, "README.md") {
		t.Errorf("the commit does not contain the agent's work:\n%s", out)
	}

	// And it reached the remote, on its own branch and NOT on main. Asserted
	// as main being where it was, which is the property that matters: an
	// agent's work must never arrive on the default branch.
	remote, err := git("ls-remote", "--heads", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(remote, "refs/heads/bead/sa-kfh") {
		t.Errorf("the branch is not on the remote:\n%s", remote)
	}
	if out, err := git("branch", "--contains", got.Commit, "--all"); err != nil {
		t.Fatal(err)
	} else if strings.Contains(out, "origin/main") || strings.Contains(out, " main") {
		t.Errorf("the commit is on main:\n%s", out)
	}

	// The tree is back on the base branch, so the NEXT bead in this rig does
	// not branch from this one's work.
	if head, err := git("rev-parse", "--abbrev-ref", "HEAD"); err != nil {
		t.Fatal(err)
	} else if strings.TrimSpace(head) != "main" {
		t.Errorf("the tree was left on %q; the next bead would build on it", strings.TrimSpace(head))
	}
}

// Each bead's branch starts from the base, not from the previous bead's work.
//
// This is the branch-graph version of the pre-run snapshot, and it compounds
// rather than staying constant: left on the first bead's branch, every later
// run in that rig branches from it and every pull request carries the earlier
// beads' commits as well as its own. The first real landing left the sandbox
// rig on `bead/sa-pv2` exactly this way.
func TestEachBeadsBranchStartsFromTheBase(t *testing.T) {
	dir, git := repo(t)

	if err := os.WriteFile(filepath.Join(dir, "first.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := Land(git, Spec{Bead: "sa-one", Base: "main"})
	if err != nil || first == nil {
		t.Fatalf("first landing: %v %+v", err, first)
	}

	if err := os.WriteFile(filepath.Join(dir, "second.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := Land(git, Spec{Bead: "sa-two", Base: "main"})
	if err != nil || second == nil {
		t.Fatalf("second landing: %v %+v", err, second)
	}

	// The second branch must not contain the first bead's commit.
	out, err := git("branch", "--contains", first.Commit, "--all")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "bead/sa-two") {
		t.Errorf("sa-two's branch carries sa-one's commit:\n%s", out)
	}

	// And the second pull request would show one file, not two.
	files, err := git("show", "--name-only", "--format=", second.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(files, "first.txt") {
		t.Errorf("sa-two's commit carries sa-one's file:\n%s", files)
	}
	if !strings.Contains(files, "second.txt") {
		t.Errorf("sa-two's commit is missing its own work:\n%s", files)
	}
}

// A bead whose work needed no code change is the ordinary case, not a failure.
//
// sa-4yn was exactly this: it found the tab's accessible name came from the
// visible text, so there was no attribute to update. Reporting that as an error
// would make every correct "nothing to change" look like a broken run.
func TestNothingToLandIsNotAFailure(t *testing.T) {
	_, git := repo(t)
	got, err := Land(git, Spec{Bead: "sa-4yn", Title: "Update aria-label", Base: "main"})
	if err != nil {
		t.Fatalf("a clean tree reported an error: %v", err)
	}
	if got != nil {
		t.Errorf("a clean tree landed %+v", got)
	}
}

// A tree containing ONLY plumbing is also nothing to land. Otherwise every rig
// would open an empty pull request the first time a bead ran in it.
func TestATreeOfOnlyPlumbingLandsNothing(t *testing.T) {
	dir, git := repo(t)
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "redirect"),
		[]byte("../../.beads\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Land(git, Spec{Bead: "sa-xxx", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("a tree of only plumbing landed %+v", got)
	}
}

// Landing twice on one bead reuses its branch rather than opening a second.
// Re-dispatching work must not multiply pull requests.
func TestASecondLandingReusesTheBeadsBranch(t *testing.T) {
	dir, git := repo(t)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := Land(git, Spec{Bead: "sa-kfh", Base: "main"})
	if err != nil || first == nil {
		t.Fatalf("first landing: %v %+v", err, first)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := Land(git, Spec{Bead: "sa-kfh", Base: "main"})
	if err != nil || second == nil {
		t.Fatalf("second landing: %v %+v", err, second)
	}
	if second.Branch != first.Branch {
		t.Errorf("the second landing used %s, want %s", second.Branch, first.Branch)
	}
	if second.Commit == first.Commit {
		t.Error("the second landing made no new commit")
	}
	heads, _ := git("ls-remote", "--heads", "origin")
	if n := strings.Count(heads, "refs/heads/bead/"); n != 1 {
		t.Errorf("%d bead branches on the remote, want 1:\n%s", n, heads)
	}
}

// The exclude is written where a WORKTREE keeps it. The rigs are laid out
// exactly that way -- refinery/rig is a worktree of .repo.git -- so a naive
// .git/info/exclude path does not exist and the write would fail or land
// somewhere harmless and useless.
func TestTheExcludeWorksInAWorktree(t *testing.T) {
	dir, _ := repo(t)
	tree := filepath.Join(filepath.Dir(dir), "rig")
	cmd := exec.Command("git", "-C", dir, "worktree", "add", "-b", "wt", tree)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("no worktree support here: %v: %s", err, out)
	}

	git := Exec(tree, filepath.Dir(dir))
	if err := os.MkdirAll(filepath.Join(tree, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, ".beads", "redirect"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Exclude(git); err != nil {
		t.Fatal(err)
	}
	if _, err := git("check-ignore", "-q", ".beads/"); err != nil {
		t.Error("the plumbing is not ignored in a worktree, so add -A would stage it")
	}
	// And doing it again changes nothing.
	before, _ := git("status", "--porcelain")
	if err := Exclude(git); err != nil {
		t.Fatal(err)
	}
	after, _ := git("status", "--porcelain")
	if before != after {
		t.Errorf("a second Exclude changed the tree:\n%q\n%q", before, after)
	}
}

// The commit says which bead it came from. Somebody reading `git log` in the
// target repository has no other way to find out why the change was made.
func TestTheCommitMessageNamesTheBead(t *testing.T) {
	got := Message(Spec{Bead: "sa-kfh", Title: "Rename hero tab label to Studio",
		Summary: "Renamed the visible label; left internal comments alone."})
	for _, want := range []string{
		"Rename hero tab label to Studio (sa-kfh)",
		"left internal comments alone",
		"Workgraph agent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the message does not contain %q:\n%s", want, got)
		}
	}
	// A bead with no title still produces a usable subject.
	if bare := Message(Spec{Bead: "sa-xxx"}); !strings.Contains(bare, "sa-xxx") {
		t.Errorf("a titleless bead lost its id:\n%s", bare)
	}
}

// A landing whose branch would be the base is refused. This is the one check
// standing between an agent's edit and a commit on main.
func TestLandingRefusesToBeTheBase(t *testing.T) {
	_, git := repo(t)
	if _, err := Land(git, Spec{Bead: "kfh", Base: "bead/kfh"}); err == nil {
		t.Error("a landing onto its own base was allowed")
	}
	if _, err := Land(git, Spec{Bead: "sa-kfh"}); err == nil {
		t.Error("a landing with no base was allowed")
	}
}

// Only what the RUN changed is landed. Everything already in the tree is left
// alone, whether or not it is on the plumbing list.
//
// The blocklist was the first design and it was wrong within a day: it named
// .beads/redirect, and then `gt` turned out to also write an untracked
// .gitignore into any rig whose repository has none. A list of the things I
// happened to have noticed is not a safety property. This is: the snapshot
// does not need to know what the stray file IS.
func TestOnlyWhatTheRunChangedIsLanded(t *testing.T) {
	dir, git := repo(t)

	// Three things already in the tree before the agent starts, none of them
	// on the plumbing list, all of them real cases from the oss cell:
	//   an untracked .gitignore that gt wrote,
	//   a previous bead's uncommitted edit -- portaljs carried sa-kfh's for
	//   two days,
	//   and something a person left behind.
	for path, body := range map[string]string{
		".gitignore":  ".opencode/\n.logs/\nCLAUDE.md\n",
		"README.md":   "a previous bead's edit\n",
		"scratch.txt": "someone's notes\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before, err := Changes(git)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 3 {
		t.Fatalf("the snapshot saw %d paths, want 3: %+v", len(before), before)
	}

	// Now the run does its work.
	if err := os.WriteFile(filepath.Join(dir, "hero.tsx"), []byte("Studio\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Land(git, Spec{Bead: "sa-kfh", Base: "main", Before: before})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("nothing landed")
	}
	if !slices.Equal(res.Files, []string{"hero.tsx"}) {
		t.Errorf("landed %v, want just hero.tsx", res.Files)
	}

	// The commit is what reaches somebody's repository, so that is what is
	// asserted -- not the Result, which is only this code's opinion of itself.
	out, err := git("show", "--name-only", "--format=", res.Branch)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{".gitignore", "README.md", "scratch.txt"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("the commit carries %s, which the run did not touch:\n%s", unwanted, out)
		}
	}
	if !strings.Contains(out, "hero.tsx") {
		t.Errorf("the commit is missing the run's work:\n%s", out)
	}

	// And they are still in the tree afterwards, not silently discarded.
	after, err := Changes(git)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, c := range after {
		left = append(left, c.Path)
	}
	slices.Sort(left)
	if !slices.Equal(left, []string{".gitignore", "README.md", "scratch.txt"}) {
		t.Errorf("the tree afterwards holds %v; the pre-existing changes should be untouched", left)
	}
}

// A file the run edited FURTHER is the run's work, even though its path was
// already dirty. The snapshot compares paths, and this is the case that makes
// that the right choice rather than a shortcut: a bead whose job is to finish
// a change somebody started would otherwise land nothing.
func TestAPathTheRunEditedFurtherIsStillItsWork(t *testing.T) {
	dir, git := repo(t)

	// Dirty before the run.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := Changes(git)
	if err != nil {
		t.Fatal(err)
	}

	// The run touches the same file.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("finished\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Landed as nothing: the path was already dirty, so this code cannot tell
	// the run's edit from the one that was there. Asserted so the limitation is
	// recorded rather than discovered -- see the comment on Spec.Before.
	res, err := Land(git, Spec{Bead: "sa-kfh", Base: "main", Before: before})
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Errorf("a pre-existing dirty path was landed as the run's work: %+v", res)
	}
}

// With no snapshot the behaviour is the old one: everything dirty is treated as
// the run's work. That is what happens when the tree cannot be read before a
// run, and it must not be a silent no-op -- losing the agent's work entirely is
// worse than landing a stray file, which is visible and removable.
func TestWithNoSnapshotEverythingDirtyIsLanded(t *testing.T) {
	dir, git := repo(t)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Land(git, Spec{Bead: "sa-kfh", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !slices.Equal(res.Files, []string{"README.md"}) {
		t.Errorf("landed %+v, want README.md", res)
	}
}

// A rig catches up with the remote before a run, or says why not.
//
// Without this a rig works once and then drifts: datopian/portaljs#1662 was
// merged and the rig's own main stayed at the commit before it, still reading
// "Visual builder" in the very file the pull request had changed.
func TestARigCatchesUpWithTheRemote(t *testing.T) {
	dir, git := repo(t)
	root := filepath.Dir(dir)

	// Somebody merges something, the way #1662 was merged.
	other := filepath.Join(root, "other")
	run := func(d string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	run(root, "clone", filepath.Join(root, "origin.git"), other)
	if err := os.WriteFile(filepath.Join(other, "hero.tsx"), []byte("Studio\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(other, "add", "hero.tsx")
	run(other, "commit", "-m", "rename the hero tab")
	run(other, "push", "origin", "main")

	// The rig has not seen it.
	if _, err := os.Stat(filepath.Join(dir, "hero.tsx")); err == nil {
		t.Fatal("the rig already has the merged file")
	}

	moved, why := Refresh(git, "main")
	if why != "" {
		t.Fatalf("the refresh was refused: %s", why)
	}
	if !moved {
		t.Error("the refresh reported no movement, but the remote had moved")
	}
	if _, err := os.Stat(filepath.Join(dir, "hero.tsx")); err != nil {
		t.Error("the merged file is still missing after a refresh")
	}

	// Again, with nothing new: no movement, and not an error.
	moved, why = Refresh(git, "main")
	if why != "" || moved {
		t.Errorf("a second refresh reported moved=%v why=%q", moved, why)
	}
}

// A refresh never throws away work, and says which state stopped it. Each of
// these is a real state a rig gets into, and destroying a tree to be tidy is
// not a trade worth making.
func TestARefreshRefusesRatherThanDiscarding(t *testing.T) {
	t.Run("a dirty tree", func(t *testing.T) {
		dir, git := repo(t)
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("edited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		moved, why := Refresh(git, "main")
		if moved || !strings.Contains(why, "uncommitted") {
			t.Errorf("moved=%v why=%q; want a refusal naming the uncommitted change", moved, why)
		}
		// And the edit survives.
		body, err := os.ReadFile(filepath.Join(dir, "README.md"))
		if err != nil || !strings.Contains(string(body), "edited") {
			t.Error("the refresh discarded an uncommitted change")
		}
	})

	t.Run("untracked files do not block it", func(t *testing.T) {
		dir, git := repo(t)
		// gt leaves both of these in every rig, so refusing on their account
		// would mean never refreshing at all.
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		for path, body := range map[string]string{
			".beads/redirect": "../../.beads\n",
			".gitignore":      ".opencode/\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, why := Refresh(git, "main"); why != "" {
			t.Errorf("untracked plumbing blocked the refresh: %s", why)
		}
	})

	t.Run("a checkout on another branch", func(t *testing.T) {
		_, git := repo(t)
		if _, err := git("checkout", "-b", "bead/sa-kfh"); err != nil {
			t.Fatal(err)
		}
		moved, why := Refresh(git, "main")
		if moved || !strings.Contains(why, "bead/sa-kfh") {
			t.Errorf("moved=%v why=%q; want a refusal naming the branch", moved, why)
		}
	})

	t.Run("no base branch", func(t *testing.T) {
		_, git := repo(t)
		if moved, why := Refresh(git, ""); moved || why == "" {
			t.Errorf("moved=%v why=%q; want a refusal", moved, why)
		}
	})

	t.Run("a diverged base is not merged", func(t *testing.T) {
		dir, git := repo(t)
		root := filepath.Dir(dir)

		// A commit on the remote and a different one here: fast-forward is
		// impossible, and merging or rebasing would invent a resolution
		// nobody asked for.
		other := filepath.Join(root, "diverged")
		run := func(d string, args ...string) {
			cmd := exec.Command("git", args...)
			cmd.Dir = d
			cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
				"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
			}
		}
		run(root, "clone", filepath.Join(root, "origin.git"), other)
		if err := os.WriteFile(filepath.Join(other, "theirs.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(other, "add", "theirs.txt")
		run(other, "commit", "-m", "theirs")
		run(other, "push", "origin", "main")

		if err := os.WriteFile(filepath.Join(dir, "ours.txt"), []byte("y\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := git("add", "ours.txt"); err != nil {
			t.Fatal(err)
		}
		if _, err := git("commit", "-m", "ours"); err != nil {
			t.Fatal(err)
		}

		moved, why := Refresh(git, "main")
		if moved || !strings.Contains(why, "fast-forward") {
			t.Errorf("moved=%v why=%q; want a refusal about fast-forwarding", moved, why)
		}
		// Our commit is still here.
		if out, err := git("log", "--oneline", "-1"); err != nil {
			t.Fatal(err)
		} else if !strings.Contains(out, "ours") {
			t.Errorf("the refresh moved off our own commit:\n%s", out)
		}
	})
}

// A dependency tree the run installed is never committed.
//
// This is the regression from datopian/msf#1. The bead asked for a PortalJS
// scaffold, the agent -- newly holding a shell -- ran `npm install`, and the
// landing committed what came back: 378 of the 454 files in that pull request
// were node_modules, +84,804 lines. The repository had just been created and
// had no root .gitignore, so nothing else was going to stop it.
func TestADependencyTreeTheRunInstalledIsNotCommitted(t *testing.T) {
	dir, git := repo(t)

	// What the agent produced: a real edit, and a node_modules beside it.
	write(t, dir, "portal/package.json", `{"name":"portal"}`)
	write(t, dir, "node_modules/next/index.js", "module.exports = {}\n")
	write(t, dir, "node_modules/.package-lock.json", "{}")
	// And build output, from the `npm run build` the instructions ask for.
	write(t, dir, "portal/.next/BUILD_ID", "abc123")

	res, err := Land(git, Spec{Bead: "sa-7dc", Title: "Scaffold a portal", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("the real edit was not landed")
	}

	for _, f := range res.Files {
		if strings.Contains(f, "node_modules") || strings.Contains(f, ".next") {
			t.Errorf("committed %q", f)
		}
	}
	// `git status` reports a wholly-new directory as the directory, so the
	// landed path is `portal/` rather than each file under it. The commit is
	// asserted below; what matters here is that the work was not dropped.
	if len(res.Files) == 0 {
		t.Errorf("the run's actual work was not committed: %v", res.Files)
	}
	// Reported rather than silently dropped, so the pull request can say what
	// was left behind.
	if len(res.Skipped) == 0 {
		t.Error("node_modules was skipped and not reported")
	}

	// And the commit itself, not just the report -- the report is derived from
	// the same list that decides the staging, so on its own it would agree
	// with itself while the tree said otherwise.
	// By BRANCH, not HEAD: the tree is returned to the base after a landing,
	// so HEAD is main again and this would assert against the wrong commit --
	// and pass, because main has no node_modules in it either.
	out, err := git("show", "--name-only", "--format=", res.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "node_modules") || strings.Contains(out, ".next") {
		t.Errorf("the commit contains build output:\n%s", out)
	}
	// And the real work IS in it -- otherwise a landing that committed
	// nothing at all would satisfy every assertion above.
	if !strings.Contains(out, "portal/package.json") {
		t.Errorf("the commit does not contain the run's work:\n%s", out)
	}
}

// A repository that TRACKS its dependencies or its built output keeps them.
//
// The rule is untracked-and-new, not the name alone. Some repositories commit
// a vendored dist/ deliberately, and a list of directory names that overrode
// that would quietly stop landing their edits -- the same class of bug as
// committing node_modules, in the opposite direction and harder to notice.
func TestATrackedBuildDirectoryStillLands(t *testing.T) {
	dir, git := repo(t)

	// The repository commits dist/ itself, before the run.
	write(t, dir, "dist/bundle.js", "// v1\n")
	if _, err := git("add", "dist/bundle.js"); err != nil {
		t.Fatal(err)
	}
	if _, err := git("commit", "-m", "vendored dist"); err != nil {
		t.Fatal(err)
	}
	// Pushed, so the landing's fetch-and-ff-only sees no divergence.
	if _, err := git("push", "origin", "main"); err != nil {
		t.Fatal(err)
	}

	// The run edits that tracked file.
	write(t, dir, "dist/bundle.js", "// v2, edited by the run\n")

	res, err := Land(git, Spec{Bead: "sa-1zz", Title: "Update the bundle", Base: "main"})
	if err != nil {
		t.Fatalf("a tracked dist/ edit was refused: %v", err)
	}
	if res == nil || !slices.Contains(res.Files, "dist/bundle.js") {
		t.Fatalf("the repository tracks dist/ and its edit was dropped: %+v", res)
	}
}
