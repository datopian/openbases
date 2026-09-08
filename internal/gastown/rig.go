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
func AddRig(ctx context.Context, gtBinary, townDir, cloneURL string) error {
	switch {
	case strings.TrimSpace(gtBinary) == "":
		return errors.New("no gt binary configured, so no rig can be created")
	case strings.TrimSpace(townDir) == "":
		return errors.New("no town directory, so there is nowhere to create a rig")
	case strings.TrimSpace(cloneURL) == "":
		return errors.New("no clone URL, so there is nothing to create a rig from")
	}

	// gt reads the town from the current directory, not from a flag. A run from
	// anywhere else finds no town and reports something unhelpful about
	// configuration -- which cost a session's debugging once already, and is
	// recorded in test/contract's fixture for the same reason.
	if st, err := os.Stat(townDir); err != nil || !st.IsDir() {
		return fmt.Errorf("town %s is not a directory: %w", townDir, err)
	}

	// Long enough for a large repository on a small node, bounded so a hung
	// clone does not hold the dispatcher's only job slot for ever.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, gtBinary, "rig", "add", cloneURL)
	cmd.Dir = townDir
	// HOME rather than the caller's environment: gt resolves the town and its
	// settings from HOME, and a dispatcher started by systemd may not have the
	// one belonging to the cell user.
	cmd.Env = append(os.Environ(), "HOME="+filepath.Dir(townDir))

	out, err := cmd.CombinedOutput()
	if err != nil {
		// gt's own message is the useful part -- "repository is empty (no
		// commits)" is a different problem from "authentication failed", and
		// both read better than an exit status. Trimmed from the end because
		// the interesting line is the last one, and usage text is long.
		return fmt.Errorf("gt rig add %s: %w: %s", cloneURL, err, tail(string(out), 400))
	}
	return nil
}

// tail keeps the end of s, which is where a command's error is.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
