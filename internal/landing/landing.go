// Package landing puts an agent's changes on a branch and pushes it.
//
// The agent does not do this and must not: its tools are Read, Grep, Glob,
// Edit, Write and `Bash(bd:*)`, so it cannot run git at all, and the deny list
// names `git push` and `gh` explicitly. That is deliberate -- an agent that can
// push can force-push main -- but it left the loop one step short of the
// repository. sa-kfh renamed a hero tab label, closed its bead correctly, and
// left
//
//	M site/components/home/LandingHero.tsx
//
// on the execution node: no commit, no branch, no pull request, and nothing in
// the interface saying so. This is the machinery that finishes the job.
//
// Nothing here talks to GitHub's API. The branch is pushed with the credential
// helper the cell already has, and the pull request is opened by the control
// plane, which holds the App key. A node that could open pull requests on its
// own would need that key on the node.
package landing

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Git runs one git command in a working tree. Injected so the tests can drive
// a real repository rather than a mock: what is being asserted here is git's
// actual behaviour, and a fake would only assert my belief about it.
type Git func(args ...string) (string, error)

// Exec is the Git a node uses.
//
// HOME matters: the credential helper is configured in the cell user's git
// configuration, and a git run without HOME does not find it -- which is how a
// bd invocation once wrote a root-owned Dolt manifest into a graph.
func Exec(dir, home string) Git {
	return func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append([]string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin"},
			// Committing needs an identity, and the cell user has none. Set
			// here rather than in the user's config so it is visible in the
			// code that makes the commit.
			"GIT_AUTHOR_NAME=Workgraph agent",
			"GIT_AUTHOR_EMAIL=agent@openbases.com",
			"GIT_COMMITTER_NAME=Workgraph agent",
			"GIT_COMMITTER_EMAIL=agent@openbases.com",
			// Never a pager, never a prompt. A landing that blocks on either
			// hangs until the deadline kills it and reports nothing useful.
			"GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("git %s: %w: %s",
				strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}
}

// Plumbing is what gastown leaves in a rig's working tree and what must never
// be committed to somebody's repository.
//
// `gt rig add` writes .beads/redirect into the refinery checkout -- present in
// all twelve rigs on the oss cell -- and it is not in the repository's
// .gitignore, because it is nothing to do with the repository. Left alone it
// shows as an untracked file beside the agent's real work, and the first
// `git add -A` would push gastown's plumbing into PortalJS.
var Plumbing = []string{".beads/"}

// Change is one path the run touched.
type Change struct {
	Path   string
	Status string // git's two-letter porcelain code, e.g. " M" or "??"
}

// Changes reports what a working tree has that its HEAD does not.
func Changes(git Git) ([]Change, error) {
	out, err := git("status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var changes []Change
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		// Porcelain v1: two status characters, a space, then the path. Renames
		// read `R  old -> new`, and the new path is what matters.
		status, path := line[:2], strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		changes = append(changes, Change{Path: strings.Trim(path, `"`), Status: status})
	}
	return changes, nil
}

// Interesting reports the changes that belong to the repository, and separately
// the plumbing that does not.
func Interesting(changes []Change) (work, plumbing []Change) {
	for _, c := range changes {
		if isPlumbing(c.Path) {
			plumbing = append(plumbing, c)
			continue
		}
		work = append(work, c)
	}
	return work, plumbing
}

func isPlumbing(path string) bool {
	for _, p := range Plumbing {
		if strings.HasPrefix(path, p) || strings.HasPrefix(path, "./"+p) {
			return true
		}
	}
	return false
}

// Exclude tells this checkout to ignore gastown's plumbing, in
// .git/info/exclude rather than .gitignore.
//
// That file is local and never committed, which is exactly right: the plumbing
// is this node's, not the repository's, and adding it to the repository's own
// .gitignore would be a change to somebody else's project to accommodate ours.
func Exclude(git Git) error {
	var wanted []string
	for _, p := range Plumbing {
		// Already ignored, by this file or by the repository's own .gitignore.
		if _, err := git("check-ignore", "-q", p); err == nil {
			continue
		}
		wanted = append(wanted, p)
	}
	if len(wanted) == 0 {
		return nil
	}

	// --git-path resolves info/exclude correctly for a worktree, where .git is
	// a file pointing elsewhere and the naive path does not exist. The rigs
	// are laid out exactly that way: refinery/rig is a worktree of .repo.git.
	out, err := git("rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	path := strings.TrimSpace(out)
	if !filepath.IsAbs(path) {
		dir, err := git("rev-parse", "--show-toplevel")
		if err != nil {
			return err
		}
		path = filepath.Join(strings.TrimSpace(dir), path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	body := string(existing)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += "# Gas Town's own plumbing, written into this checkout by `gt rig add`.\n" +
		"# Local to this node and nothing to do with this repository, so it is\n" +
		"# excluded here rather than in the repository's .gitignore.\n"
	for _, p := range wanted {
		body += p + "\n"
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

// Result is what a landing produced.
type Result struct {
	Branch  string
	Commit  string
	Files   []string
	Skipped []string // plumbing that was deliberately not committed
}

// Spec is one landing.
type Spec struct {
	Bead    string
	Title   string // the bead's title, for the commit subject
	Base    string // the branch to open the pull request against
	Summary string // the agent's own words, for the commit body
}

// Land commits what the run changed onto the bead's own branch and pushes it.
//
// Returns a nil Result and no error when there was nothing to land: a bead
// whose work needed no code change is the ordinary case, not a failure. sa-4yn
// was exactly that -- it found the accessible name came from the visible text,
// so there was no attribute to update -- and treating it as an error would make
// every correct "nothing to change" look like a broken run.
func Land(git Git, s Spec) (*Result, error) {
	if strings.TrimSpace(s.Bead) == "" {
		return nil, errors.New("a landing needs a bead")
	}
	base := strings.TrimSpace(s.Base)
	if base == "" {
		return nil, errors.New("a landing needs a base branch")
	}
	branch := Branch(s.Bead)
	if branch == base {
		// Cannot happen with the current naming, and asserted anyway: this is
		// the one check between an agent's edit and a commit on main.
		return nil, fmt.Errorf("the bead's branch and the base are both %q", branch)
	}

	// What the tree holds is read BEFORE the plumbing is excluded, so that what
	// was deliberately left out can be reported. Excluding first makes the
	// plumbing invisible to `git status` and Skipped then always reads empty --
	// a report that says nothing was skipped while something was, which is
	// worse than no report at all.
	changes, err := Changes(git)
	if err != nil {
		return nil, err
	}
	work, plumbing := Interesting(changes)
	if len(work) == 0 {
		return nil, nil
	}

	// Excluded before anything is staged, so `git add -A` cannot pick the
	// plumbing up even if the assertion below were wrong.
	if err := Exclude(git); err != nil {
		return nil, fmt.Errorf("excluding gastown's plumbing: %w", err)
	}

	// On the bead's branch. Already there when a previous run landed and the
	// tree was left on it; created from the current HEAD otherwise.
	if head, err := git("rev-parse", "--abbrev-ref", "HEAD"); err != nil {
		return nil, err
	} else if strings.TrimSpace(head) != branch {
		if _, err := git("checkout", "-b", branch); err != nil {
			// The branch may exist from an earlier landing whose push failed.
			if _, err2 := git("checkout", branch); err2 != nil {
				return nil, fmt.Errorf("cannot reach branch %s: %w", branch, err)
			}
		}
	}

	if _, err := git("add", "-A"); err != nil {
		return nil, err
	}

	// What is actually staged, checked against what must never be. `git add -A`
	// with the exclude in place should make this impossible; it is asserted
	// because the cost of being wrong is a commit of somebody else's
	// repository containing our plumbing, and the cost of the check is one
	// git invocation.
	staged, err := git("diff", "--cached", "--name-only")
	if err != nil {
		return nil, err
	}
	for _, path := range strings.Fields(staged) {
		if isPlumbing(path) {
			return nil, fmt.Errorf("refusing to commit %s: it is gastown's plumbing, "+
				"not the repository's", path)
		}
	}
	if strings.TrimSpace(staged) == "" {
		// Everything the run touched was plumbing.
		return nil, nil
	}

	if _, err := git("commit", "-m", Message(s)); err != nil {
		return nil, err
	}
	sha, err := git("rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if _, err := git("push", "--set-upstream", "origin", branch); err != nil {
		return nil, fmt.Errorf("pushing %s: %w", branch, err)
	}

	files := make([]string, 0, len(work))
	for _, c := range work {
		files = append(files, c.Path)
	}
	skipped := make([]string, 0, len(plumbing))
	for _, c := range plumbing {
		skipped = append(skipped, c.Path)
	}
	return &Result{
		Branch:  branch,
		Commit:  strings.TrimSpace(sha),
		Files:   files,
		Skipped: skipped,
	}, nil
}

// Branch is the branch a bead's work lands on.
//
// The bead id alone, with no slug from the title. A slug would need sanitising
// and would change if the title were edited, and the branch name has to be
// stable: it is what makes a second run on the same bead push to the same
// branch instead of opening a second pull request.
func Branch(bead string) string { return "bead/" + strings.TrimSpace(bead) }

// Message is the commit message.
func Message(s Spec) string {
	subject := strings.TrimSpace(s.Title)
	if subject == "" {
		subject = "work on " + s.Bead
	}
	// The bead id in the subject, because somebody reading `git log` in the
	// target repository has no other way to find out why this change was made
	// or who asked for it.
	msg := fmt.Sprintf("%s (%s)", subject, strings.TrimSpace(s.Bead))
	if summary := strings.TrimSpace(s.Summary); summary != "" {
		msg += "\n\n" + summary
	}
	return msg + "\n\nMade by a Workgraph agent for bead " + strings.TrimSpace(s.Bead) + "."
}
