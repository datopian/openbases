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

// Plumbing is what gastown is known to leave in a rig's working tree.
//
// This is a BACKSTOP, not the mechanism. What actually decides is the snapshot
// taken before the run: anything already in the tree when the agent started is
// not the agent's work, whether or not it appears here. A blocklist alone was
// the first design and it was wrong within a day -- it listed .beads/redirect,
// and then `gt` turned out to write an untracked .gitignore too, in any rig
// whose repository does not have one. A list of the things I happened to have
// noticed is not a safety property.
//
// It survives because it makes the exclude file possible, which is what keeps
// `git status` clean for the agent reading it, and because a named refusal is
// clearer than a silent omission when something does go wrong.
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

// Since reports which of `now` was not already there in `before`, and which
// was.
//
// Paths, not contents: a file the run edited further is the run's work, and a
// file that was dirty before and untouched is not. Comparing contents would
// need the whole tree in memory for no gain -- git already tells us which paths
// differ from HEAD, and the question here is only which of those are new since
// the agent started.
func Since(before, now []Change) (changed, preexisting []Change) {
	was := make(map[string]bool, len(before))
	for _, c := range before {
		was[c.Path] = true
	}
	for _, c := range now {
		if was[c.Path] {
			preexisting = append(preexisting, c)
			continue
		}
		changed = append(changed, c)
	}
	return changed, preexisting
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
	// Warning is set when the work landed but something afterwards did not,
	// so the caller reports a success that is not quite clean rather than
	// either hiding it or calling the landing a failure.
	Warning string
}

// Spec is one landing.
type Spec struct {
	Bead    string
	Title   string // the bead's title, for the commit subject
	Base    string // the branch to open the pull request against
	Summary string // the agent's own words, for the commit body

	// Before is what the working tree already held when the run started, from
	// Changes. Everything in it is left alone.
	//
	// This is what makes the landing the RUN's work rather than the tree's.
	// Without it a landing commits whatever was lying about: gt's untracked
	// .gitignore, a previous bead's uncommitted edit -- portaljs held sa-kfh's
	// change for two days -- or anything a person left behind. Each of those
	// would arrive in somebody's pull request attributed to a bead that did
	// not make it.
	Before []Change
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
	changes, kept := Since(s.Before, changes)
	work, plumbing := Interesting(changes)
	plumbing = append(plumbing, kept...)
	if len(work) == 0 {
		return nil, nil
	}

	// Excluded before anything is staged, so `git add -A` cannot pick the
	// plumbing up even if the assertion below were wrong.
	if err := Exclude(git); err != nil {
		return nil, fmt.Errorf("excluding gastown's plumbing: %w", err)
	}

	// The bead's branch, at wherever the run worked.
	//
	// -B rather than -b, so this is the same operation whether the branch is
	// new or left over from an earlier landing. `checkout -b` fails on an
	// existing branch, and the obvious fallback -- plain `checkout` -- fails
	// too when the tree is dirty in a file the branch has a different version
	// of, which is precisely the state a re-dispatch is in. Both were tried.
	//
	// No start point is given, so the branch is created at the current HEAD and
	// the run's uncommitted work carries over. That cannot conflict, which is
	// the property that makes this the one form that always works.
	//
	// The consequence, stated plainly: a re-dispatch RESETS the bead's branch
	// to the run that just happened, rather than adding to what a previous run
	// left. The pull request then shows this bead's change relative to the
	// default branch as the latest run produced it, which is the reviewable
	// thing -- a branch accumulating two runs' partial attempts is not. It
	// relies on the bead describing an outcome rather than a step, which is
	// what acceptance criteria are for and what the runner gives the agent.
	if _, err := git("checkout", "-B", branch); err != nil {
		return nil, fmt.Errorf("cannot reach branch %s: %w", branch, err)
	}

	// Staged by path, not `git add -A`.
	//
	// -A stages the whole tree, which means the commit is a function of what
	// happens to be lying about rather than of what the run did. Naming the
	// paths makes the two the same thing, and it is the only version of this
	// that stays correct when something new appears in a checkout -- which it
	// did, twice, within a day of the first one being written.
	for _, c := range work {
		if _, err := git("add", "--", c.Path); err != nil {
			return nil, err
		}
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
	// --force-with-lease, because a re-dispatch resets the branch and the
	// remote then has a commit this one does not descend from. With-lease
	// rather than --force: it refuses if the remote moved for any reason other
	// than our own last push, so somebody else's commit on the bead's branch
	// stops this rather than being overwritten.
	if _, err := git("push", "--force-with-lease", "--set-upstream", "origin", branch); err != nil {
		return nil, fmt.Errorf("pushing %s: %w", branch, err)
	}

	// Back to the base branch, so the NEXT bead in this rig starts from the
	// repository's own default rather than from this bead's work.
	//
	// Left on the bead's branch -- which is where the first real landing left
	// the sandbox rig -- every subsequent run in that rig would branch from it,
	// and each pull request would carry the previous bead's commits as well as
	// its own. That is the same contamination the pre-run snapshot prevents in
	// the working tree, one level up in the branch graph, and it compounds
	// rather than staying constant.
	//
	// After the push, deliberately: a failure here has already been preceded by
	// the work reaching the remote, so it is reported and does not fail the
	// landing. The cost is a rig whose next landing has an odd base, which is
	// visible in that pull request; the cost of the other order would be work
	// that never left the node.
	if _, err := git("checkout", base); err != nil {
		return &Result{
			Branch: branch, Commit: strings.TrimSpace(sha),
			Files: paths(work), Skipped: paths(plumbing),
			Warning: fmt.Sprintf("the working tree is still on %s: %v", branch, err),
		}, nil
	}

	return &Result{
		Branch:  branch,
		Commit:  strings.TrimSpace(sha),
		Files:   paths(work),
		Skipped: paths(plumbing),
	}, nil
}

// paths is the Change paths, in order.
func paths(changes []Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Path)
	}
	return out
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
