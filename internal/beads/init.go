package beads

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

// InitGraph creates one Beads graph on disk, ready to be written to.
//
// The same sequence the deploy-time role performs, in the same order and for
// the same reasons, so that a graph created on demand is indistinguishable from
// one created by a deploy. Every step's reason is a comment in
// roles/beads_hq/tasks/graph.yml, and the two must not drift -- an on-demand
// graph missing one of them is a graph that fails later in a way nobody
// connects to its creation.
//
// Runs as whoever calls it, and that must be the account that owns the graph
// root: `bd` writes Dolt's manifest as the running user, and a graph whose
// files belong to somebody else cannot be opened at all -- "failed to load
// database: permission denied", which the role repairs with a recursive chown
// on every run precisely because one root-run command caused it. The publisher
// already runs as that account, which is why this needs no privilege at all.
func InitGraph(ctx context.Context, bdBinary, doltBinary, dir, prefix string) error {
	switch {
	case strings.TrimSpace(bdBinary) == "":
		return errors.New("no bd binary configured, so no graph can be initialised")
	case strings.TrimSpace(doltBinary) == "":
		return errors.New("no dolt binary configured; bd init needs it and segfaults without a usable config")
	case strings.TrimSpace(dir) == "":
		return errors.New("no directory, so there is nowhere to initialise a graph")
	case strings.TrimSpace(prefix) == "":
		// Refused rather than defaulted. The prefix is in every bead id in
		// this graph and a bead id is never rewritten, so a guessed prefix is
		// permanent.
		return errors.New("no bead prefix; it appears in every id in this graph and cannot be changed later")
	}

	// Already initialised. Guarded on the config FILE rather than the .beads
	// directory, which is the distinction the role learned the hard way: a
	// failed init leaves the directory behind, so a directory-based check
	// reports a segfaulted init as a finished one.
	if _, err := os.Stat(filepath.Join(dir, ".beads", "config.yaml")); err == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// 0750: the graph holds work whose visibility the database governs, and
	// world-readable would put a restricted project's beads within reach of
	// every account on the node.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating the graph directory: %w", err)
	}

	// A partial .beads from an earlier failure, removed before retrying for
	// the same reason the role removes it: bd init will not repair one.
	if _, err := os.Stat(filepath.Join(dir, ".beads")); err == nil {
		if err := os.RemoveAll(filepath.Join(dir, ".beads")); err != nil {
			return fmt.Errorf("removing a partial graph: %w", err)
		}
	}

	// bd init wants a git repository. This one is local only and never gains a
	// remote: the durable copy is the Dolt database and its own backup, not a
	// git push.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if out, err := run(ctx, dir, "git", "init", "-q", "-b", "main", "."); err != nil {
			return fmt.Errorf("git init: %w: %s", err, out)
		}
	}

	// Dolt writes its configuration under HOME and segfaults if it cannot. The
	// service account has no home directory, which is right for a service and
	// fatal here, so HOME points at the graph -- its config then lives beside
	// the data it belongs to.
	for _, kv := range [][2]string{
		{"user.name", "workgraph"},
		{"user.email", "workgraph@datopian.com"},
	} {
		if out, err := run(ctx, dir, doltBinary, "config", "--global", "--add", kv[0], kv[1]); err != nil {
			return fmt.Errorf("dolt config %s: %w: %s", kv[0], err, out)
		}
	}

	if out, err := run(ctx, dir, bdBinary, "init", "--prefix", prefix); err != nil {
		return fmt.Errorf("bd init --prefix %s: %w: %s", prefix, err, out)
	}

	// Proves the graph answers, rather than trusting that init exiting zero
	// means it did. A graph that cannot be read is worse than one that was
	// never created, because the row registering it says it is there.
	if out, err := run(ctx, dir, bdBinary, "ready", "--json"); err != nil {
		return fmt.Errorf("the graph does not answer after init: %w: %s", err, out)
	}
	return nil
}

// run executes one step of the sequence with HOME and PATH set as the role
// sets them.
func run(ctx context.Context, dir, binary string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	// PATH so bd can find dolt, which it shells out to. The service
	// environment does not necessarily carry /usr/local/bin.
	cmd.Env = append(os.Environ(),
		"HOME="+dir,
		"PATH=/usr/local/bin:/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
