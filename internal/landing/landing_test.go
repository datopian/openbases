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

	// The commit itself, which is the thing that reaches the repository.
	out, err := git("show", "--name-only", "--format=", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".beads") {
		t.Errorf("the commit contains gastown's plumbing:\n%s", out)
	}
	if !strings.Contains(out, "README.md") {
		t.Errorf("the commit does not contain the agent's work:\n%s", out)
	}

	// And it reached the remote, on its own branch and not on main.
	remote, err := git("ls-remote", "--heads", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(remote, "refs/heads/bead/sa-kfh") {
		t.Errorf("the branch is not on the remote:\n%s", remote)
	}
	mainSHA, _ := git("rev-parse", "origin/main")
	headSHA, _ := git("rev-parse", "HEAD")
	if strings.TrimSpace(mainSHA) == strings.TrimSpace(headSHA) {
		t.Error("the work landed on main")
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
