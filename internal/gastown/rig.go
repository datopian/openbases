package gastown

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// AddRig creates one rig in a town, cloning the repository into it.
//
// A package-level function rather than a method on Orchestrator, and that is a
// deliberate reading of ADR-0005. The ADR says nothing outside this package
// invokes `gt`, which this honours, and that adding a method to the interface is
// a decision rather than a reflex. Orchestrator describes what a caller does
// with AGENTS -- dispatch, pause, nudge -- and CLIAdapter's methods are still
// unimplemented, waiting on WP-E2. Creating a rig is a setup step performed once
// per repository by the process that happens to be inside the cell, so it does
// not belong in that interface yet.
//
// Runs as whoever calls it, which must be the cell's own user: the town, its
// Dolt data and its git credential helper all belong to that account. The
// dispatcher already runs as that user, which is why this lives where it does
// rather than in the root-owned provisioner -- that one has to `sudo -u` its way
// in, and needs root to do it.
// AddRig creates one rig in a town: the checkout, its bead prefix, and the
// exclusion for gastown's own plumbing.
//
// The arguments mirror what the deploy-time provisioner passes, deliberately.
// A first version called `gt rig add <url>` with neither a name nor a prefix,
// and gt answered with its entire usage text -- because the name is positional
// and the prefix decides bead ids, which are permanent. Letting gt derive the
// prefix would have produced ids that disagree with the ones routing already
// computed from system_rigs_wanted, and a bead id cannot be corrected later.
//
// Runs as whoever calls it, which must be the cell's own user: the town, its
// Dolt data and its git credential helper all belong to that account. The
// dispatcher already runs as that user, which is why this lives here rather
// than in the root-owned provisioner -- that one has to sudo its way in.
func AddRig(ctx context.Context, gtBinary, townDir, rig, cloneURL, prefix string) error {
	switch {
	case strings.TrimSpace(gtBinary) == "":
		return errors.New("no gt binary configured, so no rig can be created")
	case strings.TrimSpace(townDir) == "":
		return errors.New("no town directory, so there is nowhere to create a rig")
	case strings.TrimSpace(rig) == "":
		return errors.New("no rig name; gt takes it positionally and derives nothing useful without it")
	case strings.TrimSpace(cloneURL) == "":
		return errors.New("no clone URL, so there is nothing to create a rig from")
	case strings.TrimSpace(prefix) == "":
		// Refused rather than defaulted. gt would derive one from the name,
		// and it would not be the prefix routing already recorded -- so the
		// rig's beads would carry ids nothing else expects, permanently.
		return errors.New("no bead prefix; gt would derive its own and the ids would " +
			"disagree with the ones already recorded for this rig")
	}

	// gt reads the town from the current directory, not from a flag. A run from
	// anywhere else finds no town and reports something unhelpful about
	// configuration.
	if st, err := os.Stat(townDir); err != nil || !st.IsDir() {
		return fmt.Errorf("town %s is not a directory: %w", townDir, err)
	}

	// Long enough for a large repository on a small node, bounded so a hung
	// clone does not hold the dispatcher's only job slot for ever.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, gtBinary, "rig", "add", rig, cloneURL, "--prefix", prefix)
	cmd.Dir = townDir
	// HOME rather than the caller's environment: gt resolves the town and its
	// settings from HOME, and a dispatcher started by systemd may not have the
	// one belonging to the cell user.
	cmd.Env = append(os.Environ(), "HOME="+filepath.Dir(townDir))

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gt rig add %s: %w: %s", rig, err, reason(string(out)))
	}

	// Belt and braces, and the same thing the provisioner does. `gt rig add`
	// writes .beads/redirect into the working tree, which is not in the
	// repository's .gitignore because it has nothing to do with the repository,
	// so it shows up beside the agent's real work as `?? .beads/`. The landing
	// path excludes it before staging anything, but a rig whose git status is
	// clean from birth is one an agent cannot be confused by.
	if err := excludePlumbing(ctx, townDir, rig); err != nil {
		// Never fatal: the checkout exists and the work can run.
		return nil
	}
	return nil
}

// excludePlumbing tells a new checkout to ignore gastown's own files.
func excludePlumbing(ctx context.Context, townDir, rig string) error {
	tree := filepath.Join(townDir, rig, "refinery", "rig")

	// refinery/rig is a WORKTREE, so .git is a file pointing elsewhere and the
	// naive path does not exist. git itself resolves it.
	out, err := exec.CommandContext(ctx, "git", "-C", tree,
		"rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return err
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return errors.New("git reported no exclude path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(tree, path)
	}

	if body, err := os.ReadFile(path); err == nil && strings.Contains(string(body), ".beads/") {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString("\n# gastown plumbing, local to this node\n.beads/\n")
	return err
}

// reason is the useful part of a failed gt command.
//
// Neither the first line nor the last, and both were tried in the provisioner
// this mirrors: gt's own errors come FIRST and are followed by its entire usage
// text, so keeping everything buries the message in forty lines of
// --sparse-checkout documentation -- which is exactly what happened on the
// first attempt here. A git failure underneath gt comes LAST, after git's
// progress output, so keeping the first line reports "Cloning into bare
// repository..." and hides the credential error below it.
//
// So: everything except the usage block, which starts at a line of "Usage:".
func reason(out string) string {
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "Usage:" {
			break
		}
		if strings.TrimSpace(line) != "" {
			kept = append(kept, strings.TrimSpace(line))
		}
	}
	joined := strings.Join(kept, "; ")
	if len(joined) > 500 {
		return "..." + joined[len(joined)-500:]
	}
	if joined == "" {
		return strings.TrimSpace(out)
	}
	return joined
}
