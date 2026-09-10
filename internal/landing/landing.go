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
	"slices"
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

// Ephemeral is what a build produces and a dependency installer downloads:
// never the run's work, whoever asked for it.
//
// This list did not need to exist until 2026-09-09, and the reason is exact.
// Until then a polecat had no shell, so it could not run `npm install` and no
// dependency tree could appear in a checkout. The hour the shell landed, the
// first agent to use it scaffolded a portal in datopian/msf -- correctly, it
// was what the bead asked for -- and the landing committed the node_modules
// that came with it: 378 of the 454 files in datopian/msf#1, +84,804 lines.
// The repository was newly created and had no root .gitignore of its own, so
// nothing else was going to stop it.
//
// Only ever applied to paths this run created and git does not track (see
// isEphemeral). A repository that deliberately commits its dependencies or its
// dist/ has them TRACKED, and this list must not start second-guessing that --
// which is why the rule is about untracked-and-new rather than about the name
// alone.
var Ephemeral = []string{
	// JavaScript
	"node_modules/", ".next/", ".nuxt/", ".svelte-kit/", ".turbo/",
	".parcel-cache/", "bower_components/",
	// Python
	"__pycache__/", ".venv/", "venv/", ".pytest_cache/", ".mypy_cache/",
	".ruff_cache/", ".tox/", "*.egg-info/",
	// Rust, Java, general build output
	"target/", "dist/", "build/", "out/",
	// Test and tool output
	"coverage/", ".nyc_output/", ".cache/", ".gradle/",
	// An agent's own scratch space is matched by SHAPE, in isEphemeral, not
	// by name here. Listing names did not work: `.verify-tmp/` was added
	// after one run committed a portal under it, and the very next agent
	// invented `.scratch-sj2/` and put three files in a pull request. There
	// is no list of names an agent might choose.
	// A tool's own log. `portal/.npm-ci.log` reached datopian/msf#1 -- the
	// agent redirected `npm ci` into it and left it behind. Matched as a
	// fragment, so `.npm-ci.log` and `npm-debug.log` both go, and only when
	// untracked and new: a repository that tracks a log file keeps it.
	"*.log",
}

// Change is one path the run touched.
type Change struct {
	Path   string
	Status string // git's two-letter porcelain code, e.g. " M" or "??"
}

// Changes reports what a working tree has that its HEAD does not.
//
// -uall, so an untracked DIRECTORY is reported as the files inside it rather
// than as itself.
//
// Without it `git status` summarises a wholly-new directory as one entry,
// `portal/`, and every comparison downstream is made against that summary. The
// consequence was total: sa-7dc's first run left portal/ in the tree, so the
// re-dispatch's Before held `portal/`, and Since then classified everything
// beneath it as pre-existing -- including the files the second run had just
// written. An agent working inside a directory that was already untracked
// could not land anything at all, and the run reported success.
//
// The cost is a longer list, and it is bounded by the same exclude that keeps
// node_modules out of the commit: Exclude writes those patterns into
// .git/info/exclude, so from the second run onwards status does not walk them.
// A first run in a fresh checkout has no dependency tree to walk yet.
func Changes(git Git) ([]Change, error) {
	out, err := git("status", "--porcelain", "-uall")
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
// the ones that do not: gastown's plumbing, and whatever the run's own build
// left behind.
func Interesting(changes []Change) (work, skip []Change) {
	for _, c := range changes {
		if isPlumbing(c.Path) || isEphemeral(c) {
			skip = append(skip, c)
			continue
		}
		work = append(work, c)
	}
	return work, skip
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

// restore brings back the paths this bead already landed that are no longer in
// the working tree, from the commit the branch reset discarded.
//
// Returns what it restored, for the report. An absent or unreadable previous
// tip restores nothing, which is the first-run case.
func restore(git Git, prevTip, base string) ([]string, error) {
	if prevTip == "" {
		return nil, nil
	}
	out, err := git("diff", "--name-only", base+"..."+prevTip)
	if err != nil {
		// The tip is gone or unreadable. Nothing to restore, and not a reason
		// to fail a landing that has real work staged.
		return nil, nil
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	root = strings.TrimSpace(root)

	var done []string
	for _, path := range strings.Fields(out) {
		// Filtered the same way the run's own work is.
		//
		// Without this, restore is a ratchet: whatever a previous run
		// committed comes back on every later run, forever, including its
		// mistakes. That is not hypothetical -- the .verify-tmp/repo/ copy
		// from one stopped run was resurrected into three consecutive pull
		// requests, and each one reported success.
		if isPlumbing(path) || matchesAny(path, Ephemeral) || isScratch(path) {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, path)); err == nil {
			// The run has its own version. It is already staged and it wins.
			continue
		}
		if _, err := git("checkout", prevTip, "--", path); err != nil {
			// One unrestorable path is not worth failing the landing over --
			// the run's own work is already staged. Reported by omission from
			// `done`.
			continue
		}
		done = append(done, path)
	}
	return done, nil
}

// onBranch reports whether HEAD is on the named branch.
//
// The question a failed checkout leaves open: did the branch move or not. git
// answers it directly, and its answer is worth more than the exit status of
// whatever hook ran afterwards.
func onBranch(git Git, name string) bool {
	out, err := git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == name
}

// beadTip is the ref carrying what this bead has already delivered.
//
// The remote-tracking ref FIRST, and that ordering is the whole fix.
//
// `checkout -B` resets the local branch to the base on every landing, so after
// a landing that failed before its commit the local ref points at the base and
// carries nothing -- while the pushed branch still holds the work, and the
// pull request is still showing it. The msf rig was in exactly that state:
// bead/sa-7dc resolved to the initial commit while origin/bead/sa-7dc held 77
// files, so reclaim and restore both consulted the empty one and the run
// reported "nothing to land" while a portal sat in the pull request.
//
// The local branch is consulted second rather than not at all, for a first
// landing that has not been pushed yet -- and because the two agree in the
// ordinary case, where the last landing succeeded.
//
// A SHA is returned, not a ref name, and that matters for the local fallback:
// `checkout -B` MOVES the local branch between the moment this is read and the
// moment restore uses it, so a name resolves to the base by then and restores
// nothing. The first version of this returned the name and the test for the
// scratch case caught it immediately -- it had been passing on a SHA captured
// before the reset, which is the behaviour to keep.
func beadTip(git Git, branch string) string {
	for _, ref := range []string{"origin/" + branch, branch} {
		out, err := git("rev-parse", "--verify", "--quiet", ref)
		if err != nil {
			continue
		}
		if sha := strings.TrimSpace(out); sha != "" {
			return sha
		}
	}
	return ""
}

// reclaim splits `kept` into the changes the bead's branch already carries and
// the ones that are genuinely somebody else's.
//
// Prefix-matched in both directions, because the two sides report at different
// granularities: `git status` names a wholly-new directory as `portal/`, while
// the branch's diff names `portal/pages/index.tsx`. Either can be the longer
// string, so a plain equality test finds nothing.
//
// A branch that does not exist yet reclaims nothing, which is the ordinary
// first-run case and not a failure.
func reclaim(git Git, branch, base string, kept []Change) (reclaimed, rest []Change) {
	tip := beadTip(git, branch)
	if tip == "" {
		return nil, kept
	}
	out, err := git("diff", "--name-only", base+"..."+tip)
	if err != nil {
		return nil, kept
	}
	mine := strings.Fields(out)
	for _, c := range kept {
		if landedUnder(c.Path, mine) {
			reclaimed = append(reclaimed, c)
			continue
		}
		rest = append(rest, c)
	}
	return reclaimed, rest
}

// landedUnder reports whether a path and any of `mine` name overlapping work.
func landedUnder(path string, mine []string) bool {
	path = strings.TrimSuffix(strings.TrimPrefix(path, "./"), "/")
	if path == "" {
		return false
	}
	for _, m := range mine {
		m = strings.TrimSuffix(m, "/")
		if m == path || strings.HasPrefix(m, path+"/") || strings.HasPrefix(path, m+"/") {
			return true
		}
	}
	return false
}

func isPlumbing(path string) bool {
	return matchesAny(path, Plumbing)
}

// isEphemeral reports whether a change is build output or installed
// dependencies that this run created.
//
// Two conditions, and the second is the one that keeps this honest:
//
//	the path is on the Ephemeral list, and
//	git does not track it -- porcelain "??".
//
// A repository that commits its own vendored dependencies or a built dist/
// has those files TRACKED, so a modification to one reads as " M" and lands
// like any other edit. The list never overrides what a repository has decided
// to keep; it only declines to ADD a dependency tree that a build left behind.
func isEphemeral(c Change) bool {
	if strings.TrimSpace(c.Status) != "??" {
		return false
	}
	return matchesAny(c.Path, Ephemeral) || isScratch(c.Path)
}

// isScratch reports whether a path is inside a directory an agent made to
// work in rather than to deliver.
//
// By shape, because names cannot be enumerated. `.verify-tmp/` went on the
// Ephemeral list after a run committed a whole portal under it; the next
// agent chose `.scratch-sj2/` and put three files into datopian/msf#6, which
// was the entire pull request. The next will choose something else.
//
// The shape that generalises: a HIDDEN directory whose name says it is
// temporary. Both halves matter. Hidden alone would catch .github and
// .claude, which repositories deliberately track; "tmp" alone would catch a
// legitimate tmp/ or a src/tmpl/ directory. And this only ever applies to
// paths that are untracked AND new (see isEphemeral), so a repository that
// commits a .tmp directory of its own keeps it.
func isScratch(path string) bool {
	for _, seg := range strings.Split(strings.Trim(strings.TrimPrefix(path, "./"), "/"), "/") {
		if !strings.HasPrefix(seg, ".") || len(seg) < 2 {
			continue
		}
		// TrimPrefix, not seg[1:]. They agree for a hidden segment and differ
		// for every other one, and that difference is what makes the check
		// above testable: with seg[1:], deleting the hidden-directory test
		// turns "tmp" into "mp" and the rule accidentally still works, so a
		// mutation that should break it does not. Written the honest way, a
		// missing hidden-directory test immediately eats a real tmp/.
		name := strings.ToLower(strings.TrimPrefix(seg, "."))
		for _, mark := range []string{"tmp", "temp", "scratch"} {
			if strings.Contains(name, mark) {
				return true
			}
		}
	}
	return false
}

// matchesAny reports whether any SEGMENT of path names one of the listed
// directories.
//
// Segments, not a prefix. The first version anchored at the path root, so
// `node_modules/` matched `node_modules/next/index.js` and missed
// `portal/node_modules/next/index.js` -- and a portal scaffold puts its
// dependencies in exactly the second place. It went unnoticed because the run
// that prompted the list happened to install at the repository root; the -uall
// change surfaced it immediately, as `git add -- portal/.next` being refused
// for a path the filter should have dropped.
//
// A leading `*` in a pattern matches a name fragment, for `*.egg-info/`.
func matchesAny(path string, patterns []string) bool {
	segments := strings.Split(strings.Trim(strings.TrimPrefix(path, "./"), "/"), "/")
	for _, p := range patterns {
		p = strings.TrimSuffix(p, "/")
		if p == "" {
			continue
		}
		fragment, isFragment := strings.CutPrefix(p, "*")
		for _, seg := range segments {
			if isFragment {
				if strings.Contains(seg, fragment) {
					return true
				}
				continue
			}
			if seg == p {
				return true
			}
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
	// Ephemeral as well as Plumbing: excluding it here keeps `git status`
	// meaningful for the NEXT run in this checkout, which would otherwise
	// start with a node_modules in its Before set and carry it forever.
	for _, p := range append(append([]string{}, Plumbing...), Ephemeral...) {
		// Already ignored, by this file or by the repository's own .gitignore.
		if _, err := git("check-ignore", "-q", p); err == nil {
			continue
		}
		// Never exclude something this repository TRACKS.
		//
		// Found by the test for it: a repository that commits a vendored
		// dist/ had `dist/` written into info/exclude here, and the very next
		// step -- `git add -- dist/bundle.js`, staging the run's real edit to
		// a tracked file -- was refused by git with
		//
		//	The following paths are ignored by one of your .gitignore files:
		//	dist
		//	hint: Use -f if you really want to add them.
		//
		// So a list meant to keep a dependency tree out of a commit would
		// have stopped that repository landing anything under dist/ at all.
		// The rule everywhere in this file is the same: the repository's own
		// choices win, and this list only declines to ADD what a build left.
		if out, err := git("ls-files", "--", p); err == nil && strings.TrimSpace(out) != "" {
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
	body += "# Gas Town's own plumbing, plus build output and installed\n" +
		"# dependencies that a run produces. Local to this node and nothing to\n" +
		"# do with this repository, so it is excluded here rather than in the\n" +
		"# repository's .gitignore -- editing somebody else's .gitignore to suit\n" +
		"# our runner would be a change to their project.\n"
	for _, p := range wanted {
		body += p + "\n"
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

// Refresh brings the checkout's base branch up to date with the remote.
//
// Without this a rig works once and then drifts. datopian/portaljs#1662 was
// merged and the rig's own main stayed at the commit before it, still reading
// "Visual builder" in the file the pull request had just changed -- so the next
// bead would branch from a stale base, produce a pull request against an old
// commit, and an agent asked to build on that merged work would not find it.
//
// Fast-forward only, and only from a clean tree on the base branch. Every other
// state is left alone and reported:
//
//	a dirty tree means a run's work or somebody's edit is in there, and
//	throwing that away to be tidy is not a trade worth making;
//
//	a checkout on some other branch means a landing did not finish, and
//	moving it would hide that;
//
//	a base that has diverged from the remote cannot fast-forward, and merging
//	or rebasing it here would invent a resolution nobody asked for.
//
// Returns whether it moved, and the reason it did not when it did not.
func Refresh(git Git, base string) (moved bool, why string) {
	if strings.TrimSpace(base) == "" {
		return false, "no base branch is known"
	}
	head, err := git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return false, err.Error()
	}
	if strings.TrimSpace(head) != base {
		return false, fmt.Sprintf("the checkout is on %s, not %s", strings.TrimSpace(head), base)
	}

	// Tracked changes only. An untracked file -- gt's .beads/ and .gitignore
	// are in every rig -- does not stop a fast-forward and never conflicts
	// with one, so refusing on their account would mean never refreshing.
	dirty, err := git("diff", "--name-only")
	if err != nil {
		return false, err.Error()
	}
	if strings.TrimSpace(dirty) != "" {
		return false, "the tree has uncommitted changes: " + strings.Join(strings.Fields(dirty), " ")
	}

	if _, err := git("fetch", "--quiet", "origin", base); err != nil {
		return false, err.Error()
	}
	before, err := git("rev-parse", "HEAD")
	if err != nil {
		return false, err.Error()
	}
	// --ff-only, so a diverged base fails here rather than being merged.
	if _, err := git("merge", "--ff-only", "FETCH_HEAD"); err != nil {
		return false, "cannot fast-forward: " + err.Error()
	}
	after, err := git("rev-parse", "HEAD")
	if err != nil {
		return false, err.Error()
	}
	return strings.TrimSpace(before) != strings.TrimSpace(after), ""
}

// Result is what a landing produced.
type Result struct {
	Branch string
	Commit string
	Files  []string
	// Plumbing and build output that were deliberately not committed, so a
	// caller can say what was left out rather than silently dropping it.
	Skipped []string
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

	// Unstage whatever a previous attempt left, BEFORE the tree is read.
	//
	// A landing that failed after `checkout -B` used to leave the index
	// staged, and that poisoned every run after it. `git status` reports a
	// leftover staged file as `A `, not `??`, so isEphemeral -- which keys on
	// `??` -- classified portal/.npm-ci.log as the run's work, staged it,
	// tripped the commit-time assertion, failed, and left the same index
	// behind for the next run. sa-7dc could not land again, at all, and each
	// attempt reported the same refusal about a file no run had chosen.
	//
	// So the index is made to match HEAD first and the status is honest: a
	// leftover reads as untracked, which is what it is. Errors are ignored on
	// purpose -- a repository with no commits has nothing staged, and this
	// must not be the thing that stops a landing.
	_, _ = git("reset", "--quiet")

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

	// A path this bead already landed is this bead's work, even though it was
	// in the tree before this run started.
	//
	// Without this a re-dispatch DELETES what the bead had delivered, and it
	// did. sa-7dc's first run wrote a portal, landed it, and was killed at its
	// deadline with the bead still open; the re-dispatch then found portal/
	// sitting untracked in the tree -- the first run had left it there -- so
	// Since correctly called it pre-existing and did not stage it, while the
	// `checkout -B` below correctly reset the branch to the base and discarded
	// the commit that held it. Each half is right on its own. Together they
	// turned a pull request containing a working portal into one containing a
	// scratch directory, and the run reported success.
	//
	// So the bead's own branch is consulted: anything it already carries is
	// moved back out of `kept` and staged again from the tree. Done here
	// rather than by starting the branch at its previous tip, which was
	// considered and is worse -- checking out a branch that tracks portal/
	// while the tree holds an untracked portal/ is exactly the "untracked
	// working tree files would be overwritten" refusal that the no-start-point
	// form of `checkout -B` exists to avoid.
	reclaimed, kept := reclaim(git, branch, base, kept)
	changes = append(changes, reclaimed...)

	// The bead's previous tip, read before `checkout -B` discards it.
	//
	// Reclaiming from the TREE is not enough on its own, and the test for it
	// showed why: a file the previous run committed is removed from the
	// working tree when the landing returns to the base branch, so it is not
	// on disk to be staged again. Only the commit still has it.
	prevTip := beadTip(git, branch)

	work, skip := Interesting(changes)
	skip = append(skip, kept...)
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
	// Registered BEFORE the checkout, not after it.
	//
	// Every exit from here has to put the tree back -- including the ones that
	// return an error, and including a checkout that FAILS. `checkout -B`
	// switches the branch and then runs post-checkout hooks, so a failure can
	// leave the tree already switched: sa-fj3 aborted here and left the msf
	// rig sitting on bead/sa-fj3, because the cleanup was registered on the
	// line after the call that had just moved it.
	defer func() {
		_, _ = git("reset", "--quiet")
		_, _ = git("checkout", base)
	}()

	if _, err := git("checkout", "-B", branch); err != nil {
		// A hook that complains is not a failed checkout.
		//
		// git runs post-checkout hooks after switching, and reports THEIR exit
		// status as its own. The msf repository is a PortalJS portal, so it
		// ships Git LFS hooks, and on a node without the git-lfs binary every
		// checkout ends:
		//
		//	Switched to and reset branch 'bead/sa-fj3'
		//	...
		//	This repository is configured for Git LFS but 'git-lfs' was not
		//	found on your path.
		//
		// exit status 1. The switch had happened; the landing threw the run's
		// work away anyway, and would have done so on every future run of that
		// repository. Hooks are the repository's, not ours, and they fail for
		// reasons that have nothing to do with whether the branch moved.
		//
		// So the outcome is asked of git rather than inferred from the exit
		// code -- and if the branch really was not reached, the original error
		// is still what the caller sees.
		if !onBranch(git, branch) {
			return nil, fmt.Errorf("cannot reach branch %s: %w", branch, err)
		}
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

	// Then whatever this bead had already delivered and is no longer on disk.
	//
	// AFTER staging, and only for paths the working tree does not have: where
	// the run rewrote a file the previous attempt had landed, the run's
	// version is already staged and wins. Where the run never touched it, the
	// earlier delivery is restored from the commit that `checkout -B` just
	// discarded.
	//
	// This is what stops a re-dispatch from being a deletion. It is also the
	// narrowest form that cannot conflict: `git checkout <tip> -- <path>` for
	// a path absent from the tree has nothing to overwrite, which is the
	// property that ruled out simply starting the branch at its previous tip.
	//
	// One consequence, stated because it is a real choice: a run that
	// deliberately DELETED a file an earlier run of the same bead added will
	// see it restored. Ambiguous either way, and the resolution favours not
	// losing work.
	restored, err := restore(git, prevTip, base)
	if err != nil {
		return nil, err
	}
	// Reported as landed files, because that is what they are: the pull
	// request contains them, and a report listing only this run's edits would
	// describe a smaller change than the branch actually holds.
	landed := paths(work)
	landed = append(landed, restored...)
	slices.Sort(landed)
	landed = slices.Compact(landed)

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
		// The backstop for a directory sweep: `git add -- portal/` stages
		// everything beneath it, so an ephemeral child can arrive through a
		// parent that is not itself ephemeral.
		//
		// Asked of HEAD rather than of the porcelain status, because by here
		// the path IS in the index and its status no longer says where it came
		// from. A path already in HEAD is one the repository tracks, and its
		// edit lands like any other -- the first version of this check omitted
		// that and refused a vendored dist/bundle.js the run had legitimately
		// edited, which the test for it caught.
		if matchesAny(path, Ephemeral) || isScratch(path) {
			if _, err := git("cat-file", "-e", "HEAD:"+path); err != nil {
				return nil, fmt.Errorf("refusing to commit %s: it is build output or "+
					"installed dependencies the run produced, not its work", path)
			}
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
			Files: landed, Skipped: paths(skip),
			Warning: fmt.Sprintf("the working tree is still on %s: %v", branch, err),
		}, nil
	}

	return &Result{
		Branch:  branch,
		Commit:  strings.TrimSpace(sha),
		Files:   landed,
		Skipped: paths(skip),
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
