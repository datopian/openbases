package landing

import (
	"errors"
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
	// Named per file rather than as `.beads/`, because Changes now asks for
	// -uall: an untracked directory is reported as its contents. The
	// granularity changed deliberately -- summarising a new directory as one
	// entry is what let Before mask the files a re-dispatch had just written
	// -- and what matters here is unchanged, that the plumbing is skipped and
	// said to be skipped.
	if !slices.Equal(got.Skipped, []string{".beads/redirect"}) {
		t.Errorf("skipped %v, want the plumbing file", got.Skipped)
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

// A re-dispatch keeps what the bead already delivered.
//
// This is sa-7dc, exactly as it happened. The first run wrote a portal, landed
// it, and was killed at its deadline with the bead still open. The re-dispatch
// found portal/ sitting untracked in the tree, because the first run had left
// it there -- so Since called it pre-existing and did not stage it, while
// `checkout -B` reset the branch to the base and discarded the commit that
// held it.
//
// Both halves were right on their own. Together the second run turned a pull
// request containing a working portal into one containing a scratch directory,
// and reported success: datopian/msf#1 went from 454 files to 76, none of them
// the portal.
func TestARedispatchKeepsWhatTheBeadAlreadyDelivered(t *testing.T) {
	dir, git := repo(t)

	// First run: writes the portal, lands it.
	write(t, dir, "portal/pages/index.tsx", "export default function Home() {}\n")
	first, err := Land(git, Spec{Bead: "sa-7dc", Title: "Scaffold a portal", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("the first run landed nothing")
	}

	// The state the second run starts in, and the premise of the whole bug.
	//
	// The tree is back on main, so the files the first run COMMITTED are gone
	// from it -- but the directory survives, because the run also left things
	// in it that were never committed. On staging that was a build artefact
	// and a public/ folder. `git status` then reports the DIRECTORY, `portal/`,
	// and that single entry is what lands in Before.
	write(t, dir, "portal/public/logo.svg", "<svg/>\n")

	before, err := Changes(git)
	if err != nil {
		t.Fatal(err)
	}
	var sawPortal bool
	for _, c := range before {
		if strings.HasPrefix(c.Path, "portal") {
			sawPortal = true
		}
	}
	if !sawPortal {
		t.Fatalf("the premise does not hold: nothing under portal/ is in the "+
			"tree, so Before cannot mask it: %+v", before)
	}

	// Second run: adds to the portal, and leaves a scratch directory of its
	// own, as the real agent did.
	//
	// The new page is the sharp end. Because Before holds `portal/` as one
	// entry, a path-level comparison classifies everything beneath it as
	// pre-existing -- including a file that did not exist when the run
	// started.
	write(t, dir, "portal/pages/search.tsx", "export default function Search() {}\n")
	write(t, dir, ".verify-tmp/out.txt", "checked\n")

	second, err := Land(git, Spec{
		Bead: "sa-7dc", Title: "Scaffold a portal", Base: "main",
		Before: before,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second == nil {
		t.Fatal("the second run landed nothing")
	}

	// The branch must still carry the portal. By branch, not HEAD.
	out, err := git("show", "--name-only", "--format=", second.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "portal/pages/index.tsx") {
		t.Errorf("the re-dispatch dropped what the bead had delivered:\n%s", out)
	}

	// And the whole branch, not just its tip commit -- the tip is what the
	// second run added, and the question is what a reviewer sees against main.
	diff, err := git("diff", "--name-only", "main..."+second.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "portal/pages/index.tsx") {
		t.Errorf("the pull request would not show the portal:\n%s", diff)
	}
	// And the second run's own new work inside that masked directory.
	if !strings.Contains(diff, "portal/pages/search.tsx") {
		t.Errorf("the second run's work inside portal/ was skipped as "+
			"pre-existing:\n%s", diff)
	}
}

// Somebody else's uncommitted work is still left alone.
//
// The counterpart to the test above, and the reason reclaim consults the
// bead's own branch rather than simply landing everything it finds. portaljs
// held sa-kfh's uncommitted change for two days; a landing that swept the tree
// would have put it in another bead's pull request.
func TestAnotherBeadsLeftoverIsStillNotLanded(t *testing.T) {
	dir, git := repo(t)

	// A previous, unrelated bead's edit, left in the tree.
	write(t, dir, "someone-elses.txt", "not mine\n")
	before, err := Changes(git)
	if err != nil {
		t.Fatal(err)
	}

	write(t, dir, "mine.txt", "my work\n")
	res, err := Land(git, Spec{
		Bead: "sa-999", Title: "Do my own work", Base: "main", Before: before,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("nothing landed")
	}
	if slices.Contains(res.Files, "someone-elses.txt") {
		t.Errorf("landed another bead's leftover: %v", res.Files)
	}
	if !slices.Contains(res.Files, "mine.txt") {
		t.Errorf("did not land its own work: %v", res.Files)
	}
}

// Dependencies and build output are skipped wherever they sit, not only at the
// repository root.
//
// The first version of the Ephemeral filter was anchored at the path root, so
// `node_modules/` matched `node_modules/next/index.js` and missed
// `portal/node_modules/next/index.js`. It went unnoticed because the run that
// prompted the list happened to install at the root -- and a PortalJS
// scaffold puts its dependencies in a subdirectory, which is the actual shape
// of the repository this was written for.
func TestBuildOutputIsSkippedAtAnyDepth(t *testing.T) {
	dir, git := repo(t)

	write(t, dir, "portal/package.json", `{"name":"portal"}`)
	write(t, dir, "portal/node_modules/next/index.js", "module.exports = {}\n")
	write(t, dir, "portal/.next/BUILD_ID", "abc")
	write(t, dir, "apps/web/dist/bundle.js", "// built\n")
	write(t, dir, "services/api/__pycache__/main.cpython-312.pyc", "\x00")

	res, err := Land(git, Spec{Bead: "sa-dep", Title: "Scaffold", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("nothing landed")
	}

	out, err := git("show", "--name-only", "--format=", res.Branch)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"node_modules", ".next", "dist/", "__pycache__"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("committed %s from a subdirectory:\n%s", unwanted, out)
		}
	}
	if !strings.Contains(out, "portal/package.json") {
		t.Errorf("the run's work was not committed:\n%s", out)
	}
}

// Restore does not resurrect an earlier run's scratch directory.
//
// Without this, restore is a ratchet: whatever a previous run committed comes
// back on every later run, forever, including its mistakes. One stopped run
// scaffolded a portal under .verify-tmp/repo/ and its 76 files were then
// restored into three consecutive pull requests, each reporting success, while
// the portal the agent had actually built sat unlanded in the tree.
func TestRestoreDoesNotResurrectAnEarlierRunsScratch(t *testing.T) {
	dir, git := repo(t)

	// A previous run of this bead committed scratch alongside one real file.
	write(t, dir, ".verify-tmp/repo/portal/pages/index.tsx", "export default function H() {}\n")
	write(t, dir, "KEEP.md", "real work\n")
	if _, err := git("checkout", "-B", Branch("sa-7dc")); err != nil {
		t.Fatal(err)
	}
	if _, err := git("add", "-f", ".verify-tmp", "KEEP.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := git("commit", "-m", "an earlier run"); err != nil {
		t.Fatal(err)
	}
	if _, err := git("checkout", "main"); err != nil {
		t.Fatal(err)
	}

	// This run does something small and unrelated.
	write(t, dir, "NOTES.md", "this run\n")

	res, err := Land(git, Spec{Bead: "sa-7dc", Title: "Scaffold", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("nothing landed")
	}

	out, err := git("show", "--name-only", "--format=", res.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".verify-tmp") {
		t.Errorf("an earlier run's scratch was restored:\n%s", out)
	}
	// The real file from the earlier run is still restored -- that is what
	// restore is for, and the filter must not throw it out with the scratch.
	if !strings.Contains(out, "KEEP.md") {
		t.Errorf("restore dropped the earlier run's real work:\n%s", out)
	}
	if !strings.Contains(out, "NOTES.md") {
		t.Errorf("this run's own work was not landed:\n%s", out)
	}
}

// A log a tool wrote is not the run's work.
//
// `portal/.npm-ci.log` reached datopian/msf#1: the agent redirected `npm ci`
// into it and left it behind, so it arrived in somebody's repository beside
// the portal.
func TestAToolsLogIsNotCommitted(t *testing.T) {
	dir, git := repo(t)

	write(t, dir, "portal/package.json", `{"name":"portal"}`)
	write(t, dir, "portal/.npm-ci.log", "npm WARN deprecated\n")
	write(t, dir, "npm-debug.log", "trace\n")

	res, err := Land(git, Spec{Bead: "sa-log", Title: "Scaffold", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("nothing landed")
	}
	out, err := git("show", "--name-only", "--format=", res.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".log") {
		t.Errorf("a tool's log was committed:\n%s", out)
	}
	if !strings.Contains(out, "portal/package.json") {
		t.Errorf("the run's work was not committed:\n%s", out)
	}

	// But a log the repository TRACKS keeps landing its edits, like anything
	// else it has decided to keep.
	write(t, dir, "audit.log", "v1\n")
	if _, err := git("add", "-f", "audit.log"); err != nil {
		t.Fatal(err)
	}
	if _, err := git("commit", "-m", "the repository tracks this log"); err != nil {
		t.Fatal(err)
	}
	if _, err := git("push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "audit.log", "v2, edited by the run\n")

	res, err = Land(git, Spec{Bead: "sa-log2", Title: "Update the log", Base: "main"})
	if err != nil {
		t.Fatalf("a tracked log's edit was refused: %v", err)
	}
	if res == nil || !slices.Contains(res.Files, "audit.log") {
		t.Fatalf("the repository tracks audit.log and its edit was dropped: %+v", res)
	}
}

// A landing that failed does not stop every landing after it.
//
// This is the wedge on the msf rig, reproduced. A landing failed at the
// commit-time assertion AFTER staging, and nothing put the tree back: it was
// left on bead/sa-7dc with 77 files staged, portal/.npm-ci.log among them.
//
// That poisoned every run after it. `git status` reports a leftover staged
// file as `A `, not `??`, so isEphemeral -- which keys on `??` -- read the npm
// log as the run's work, staged it, tripped the same assertion, failed, and
// left the same index behind. The bead could not land again at all, and each
// attempt reported a refusal about a file no run had chosen to commit.
func TestAFailedLandingDoesNotWedgeTheRig(t *testing.T) {
	dir, git := repo(t)

	// Exactly the state found on the node: on the bead's branch, with the
	// run's work staged and a tool's log staged with it.
	write(t, dir, "portal/package.json", `{"name":"portal"}`)
	write(t, dir, "portal/.npm-ci.log", "npm WARN\n")
	if _, err := git("checkout", "-B", Branch("sa-7dc")); err != nil {
		t.Fatal(err)
	}
	if _, err := git("add", "-f", "portal"); err != nil {
		t.Fatal(err)
	}

	// The premise: the leftover reads as staged, not untracked, which is what
	// made the classifier get it wrong.
	before, err := git("status", "--porcelain", "-uall")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, "A  portal/.npm-ci.log") {
		t.Fatalf("the premise does not hold: the log is not left staged:\n%s", before)
	}

	// A landing now, from that state.
	res, err := Land(git, Spec{Bead: "sa-7dc", Title: "Scaffold", Base: "main"})
	if err != nil {
		t.Fatalf("a landing after a failed one was refused, which is the wedge: %v", err)
	}
	if res == nil {
		t.Fatal("nothing landed")
	}

	out, err := git("show", "--name-only", "--format=", res.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".npm-ci.log") {
		t.Errorf("the leftover log was committed:\n%s", out)
	}
	if !strings.Contains(out, "portal/package.json") {
		t.Errorf("the real work was not committed:\n%s", out)
	}

	// And the tree is back where a run expects to find it: on the base
	// branch, with nothing staged. Otherwise the NEXT run inherits this one's
	// branch and index, which is how one failure became permanent.
	head, err := git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(head) != "main" {
		t.Errorf("the tree was left on %q, not the base branch", strings.TrimSpace(head))
	}
	staged, err := git("diff", "--cached", "--name-only")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(staged) != "" {
		t.Errorf("the index was left staged, which poisons the next run:\n%s", staged)
	}
}

// A landing that fails PART WAY THROUGH still puts the tree back.
//
// The success path always returned to the base branch, so the gap was
// invisible until something failed between `checkout -B` and the end: the msf
// rig was left on bead/sa-7dc with 77 files staged, and every run after it
// began on the wrong branch with the wrong index.
//
// Driven by making the commit fail, because with the classifier fixed there is
// no longer an input that trips the assertion -- and defence that only works
// while nothing else can go wrong is not defence.
func TestALandingThatFailsPartWayThroughStillPutsTheTreeBack(t *testing.T) {
	dir, real := repo(t)

	write(t, dir, "portal/package.json", `{"name":"portal"}`)

	// Everything works except the commit.
	git := func(args ...string) (string, error) {
		if len(args) > 0 && args[0] == "commit" {
			return "", errors.New("disk full, or a hook said no")
		}
		return real(args...)
	}

	if _, err := Land(git, Spec{Bead: "sa-7dc", Title: "Scaffold", Base: "main"}); err == nil {
		t.Fatal("a failed commit must be reported, not swallowed")
	}

	// The tree is on the base branch, with nothing staged: the state the next
	// run expects, rather than this run's leftovers.
	head, err := real("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(head) != "main" {
		t.Errorf("the tree was left on %q; the next run would start there", strings.TrimSpace(head))
	}
	staged, err := real("diff", "--cached", "--name-only")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(staged) != "" {
		t.Errorf("the index was left staged; the next run reads these as `A ` "+
			"rather than `??` and misclassifies them:\n%s", staged)
	}

	// And the run's work is still in the working tree. A cleanup that reverted
	// the agent's files would be worse than the wedge it fixes.
	if _, err := os.Stat(filepath.Join(dir, "portal", "package.json")); err != nil {
		t.Errorf("the cleanup threw away the run's work: %v", err)
	}
}

// What the bead delivered is read from the PUSHED branch, not the local one.
//
// The msf rig again, and the reason "nothing to land" was reported while a
// portal sat in the pull request. `checkout -B` resets the LOCAL branch to the
// base on every landing, so a landing that failed before its commit leaves the
// local ref carrying nothing -- while the pushed branch still holds the work.
// reclaim and restore consulted the local one:
//
//	bead/sa-7dc        -> 46b1feb (the initial commit), 0 files
//	origin/bead/sa-7dc -> f783859, 77 files
func TestWhatTheBeadDeliveredIsReadFromThePushedBranch(t *testing.T) {
	dir, git := repo(t)

	// A landing that delivered, and pushed.
	write(t, dir, "portal/pages/index.tsx", "export default function H() {}\n")
	first, err := Land(git, Spec{Bead: "sa-7dc", Title: "Scaffold", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("the first landing delivered nothing")
	}

	// Now the state a failed landing leaves: the local branch reset to the
	// base, while the pushed branch keeps the work.
	if _, err := git("branch", "-f", Branch("sa-7dc"), "main"); err != nil {
		t.Fatal(err)
	}
	local, err := git("rev-parse", Branch("sa-7dc"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := git("rev-parse", "main")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(local) != strings.TrimSpace(base) {
		t.Fatalf("the premise does not hold: the local branch was not reset")
	}
	remote, err := git("rev-parse", "origin/"+Branch("sa-7dc"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(remote) == strings.TrimSpace(base) {
		t.Fatalf("the premise does not hold: the pushed branch has nothing on it")
	}

	// A later run does something small.
	write(t, dir, "NOTES.md", "this run\n")

	res, err := Land(git, Spec{Bead: "sa-7dc", Title: "Scaffold", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("nothing landed, which is what the rig reported while the pull " +
			"request held a portal")
	}
	diff, err := git("diff", "--name-only", "main..."+res.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "portal/pages/index.tsx") {
		t.Errorf("the delivered work was not read from the pushed branch:\n%s", diff)
	}
	if !strings.Contains(diff, "NOTES.md") {
		t.Errorf("this run's own work was dropped:\n%s", diff)
	}
}

// A hook that complains is not a failed checkout.
//
// git runs post-checkout hooks after switching branches and reports THEIR exit
// status as its own. The msf repository is a PortalJS portal, so it ships Git
// LFS hooks, and on a node without the git-lfs binary every checkout ends:
//
//	Switched to and reset branch 'bead/sa-fj3'
//	...
//	This repository is configured for Git LFS but 'git-lfs' was not found
//	on your path.
//
// exit status 1. The branch had moved. The landing threw the run's work away
// anyway, left the rig sitting on the bead's branch, and would have done the
// same on every future run of that repository.
func TestAComplainingHookDoesNotThrowAwayTheWork(t *testing.T) {
	dir, real := repo(t)
	write(t, dir, "portal/package.json", `{"name":"portal"}`)

	// git, but every checkout also reports a hook failure -- after doing the
	// checkout, which is the part that matters.
	git := func(args ...string) (string, error) {
		out, err := real(args...)
		if len(args) > 0 && args[0] == "checkout" && err == nil {
			return out, errors.New("exit status 1: This repository is configured " +
				"for Git LFS but 'git-lfs' was not found on your path")
		}
		return out, err
	}

	res, err := Land(git, Spec{Bead: "sa-fj3", Title: "Deploy it", Base: "main"})
	if err != nil {
		t.Fatalf("a hook's complaint threw away the run's work: %v", err)
	}
	if res == nil {
		t.Fatal("nothing landed")
	}
	out, err := real("show", "--name-only", "--format=", res.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "portal/package.json") {
		t.Errorf("the work is not on the branch:\n%s", out)
	}

	// And a checkout that genuinely does not switch is still fatal. Asserted
	// because the fix is "ask git where HEAD is" and the failure it must keep
	// catching is HEAD not being there.
	stuck := func(args ...string) (string, error) {
		if len(args) > 1 && args[0] == "checkout" && args[1] == "-B" {
			return "", errors.New("exit status 1: fatal: cannot lock ref")
		}
		return real(args...)
	}
	write(t, dir, "another.txt", "x\n")
	_, err = Land(stuck, Spec{Bead: "sa-zzz", Title: "Nope", Base: "main"})
	if err == nil {
		t.Fatal("a checkout that did not switch must still be an error: landing " +
			"onto whatever branch the tree happens to be on is how work reaches " +
			"the wrong pull request")
	}
	// Named for what it is, and asserted rather than assumed.
	//
	// Without the guard this still fails -- but later and elsewhere, when the
	// push cannot find a local branch nobody created. The first version of
	// this check accepted that, so it passed with the guard removed and was
	// testing an accident rather than the rule. The property is that a
	// checkout which did not switch is reported AS a checkout that did not
	// switch.
	if !strings.Contains(err.Error(), "cannot reach branch") {
		t.Errorf("the failure is not attributed to the checkout: %v", err)
	}
}

// Scratch is recognised by shape, because names cannot be enumerated.
//
// `.verify-tmp/` was added to the list after one run committed an entire
// portal under it. The very next agent chose `.scratch-sj2/` and put three
// files into datopian/msf#6 — which was the whole pull request. The one after
// that will choose something else.
func TestScratchIsRecognisedByShapeNotByName(t *testing.T) {
	// Names no list would have had.
	for _, p := range []string{
		".scratch-sj2/build-geo.log",
		".verify-tmp/repo/portal/package.json",
		".tmp-run-4/out.txt",
		".agent-temp/notes.md",
		"portal/.scratchpad/x",
	} {
		if !isScratch(p) {
			t.Errorf("%q is not recognised as scratch", p)
		}
	}

	// Hidden directories a repository deliberately tracks, and ordinary paths
	// that merely contain the letters. Both halves of the rule matter: hidden
	// alone would eat .github, and "tmp" alone would eat a real tmp/.
	for _, p := range []string{
		".github/workflows/ci.yml",
		".claude/commands/portaljs-deploy.md",
		"portal/lib/templates/index.ts",
		"tmp/keepme.txt",
		"src/tmpl/page.tsx",
		".gitignore",
	} {
		if isScratch(p) {
			t.Errorf("%q was mistaken for scratch", p)
		}
	}
}

// And the shape rule reaches a landing, not just the helper.
func TestAnInventedScratchDirectoryDoesNotReachThePullRequest(t *testing.T) {
	dir, git := repo(t)

	write(t, dir, "portal/pages/search.tsx", "export default function S() {}\n")
	// Exactly what sa-sj2 did.
	write(t, dir, ".scratch-sj2/build-geo.log", "...\n")
	write(t, dir, ".scratch-sj2/notes.md", "...\n")

	res, err := Land(git, Spec{Bead: "sa-sj2", Title: "Preview", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("nothing landed")
	}
	out, err := git("show", "--name-only", "--format=", res.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".scratch-sj2") {
		t.Errorf("the agent's scratch directory reached the commit:\n%s", out)
	}
	if !strings.Contains(out, "portal/pages/search.tsx") {
		t.Errorf("the real work was not committed:\n%s", out)
	}
}
