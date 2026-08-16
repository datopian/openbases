package witness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Town reads a Gas Town installation and runs its commands.
//
// Every read is a documented Gas Town output and every write is a documented
// Gas Town command. Nothing here parses Gas Town's internal state files beyond
// the two it publishes for exactly this purpose — rigs.json and the heartbeat
// directory — because reaching further in would make us a fork of an
// orchestrator we deliberately do not own (ADR-0005).
type Town struct {
	// Root is the town directory, e.g. /srv/cells/oss/town.
	Root string
	// GT is the gt binary. Pinned by versions.lock, absolute so that a
	// truncated PATH under systemd cannot silently select a different one.
	GT string
	// Timeout bounds a single gt invocation. gt talks to Dolt, and a Dolt that
	// is starting up can block for a long time; a witness that hangs is a
	// witness that is not watching.
	Timeout time.Duration
}

// Rig is one entry of the town's rigs.json.
type Rig struct {
	Name string
	// Prefix is the rig's bead prefix ("sa"), which is also how its heartbeat
	// files are named.
	Prefix string
	// Repository is "owner/name" derived from the rig's git URL. It is how the
	// control plane finds the project that owns the work, so that the mapping
	// lives in the registry rather than in a second copy here.
	Repository string
}

type rigsFile struct {
	Rigs map[string]struct {
		GitURL string `json:"git_url"`
		Beads  struct {
			Prefix string `json:"prefix"`
		} `json:"beads"`
	} `json:"rigs"`
}

// repositoryFromGitURL turns a clone URL into "owner/name".
//
// Handles both forms Gas Town stores — https://host/owner/name.git and
// git@host:owner/name.git — and returns "" for anything else rather than a
// half-parsed guess, since a wrong repository would file an escalation against
// the wrong project.
func repositoryFromGitURL(u string) string {
	s := strings.TrimSpace(u)
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if j := strings.Index(s, "/"); j >= 0 {
			s = s[j+1:] // drop the host
		} else {
			return ""
		}
	} else if i := strings.Index(s, ":"); i >= 0 && strings.Contains(s[:i], "@") {
		s = s[i+1:] // scp form: everything after the colon
	} else {
		return ""
	}
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return ""
	}
	// The last two segments, so a self-hosted path prefix does not confuse it.
	owner, name := parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}

// Rigs lists the rigs in the town, sorted, so a pass is deterministic and its
// log is diffable between runs.
func (t Town) Rigs() ([]Rig, error) {
	raw, err := os.ReadFile(filepath.Join(t.Root, "rigs.json"))
	if err != nil {
		return nil, fmt.Errorf("reading rigs.json: %w", err)
	}
	var f rigsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parsing rigs.json: %w", err)
	}
	rigs := make([]Rig, 0, len(f.Rigs))
	for name, r := range f.Rigs {
		// A rig with no bead prefix cannot have its heartbeats located, and
		// guessing the prefix from the name would silently watch the wrong
		// files. Skipping it is visible in the tally; guessing would not be.
		if r.Beads.Prefix == "" {
			continue
		}
		rigs = append(rigs, Rig{
			Name:       name,
			Prefix:     r.Beads.Prefix,
			Repository: repositoryFromGitURL(r.GitURL),
		})
	}
	sort.Slice(rigs, func(i, j int) bool { return rigs[i].Name < rigs[j].Name })
	return rigs, nil
}

// Polecats runs `gt polecat list <rig> --json`.
func (t Town) Polecats(ctx context.Context, rig string) ([]Polecat, error) {
	out, err := t.run(ctx, "polecat", "list", rig, "--json")
	if err != nil {
		return nil, err
	}
	// gt prints advisory warnings on stdout ahead of the JSON in some
	// configurations — a .beads directory with loose permissions produces one.
	// Trimming to the first bracket keeps a cosmetic warning from reading as a
	// parse failure, which would take the whole rig out of the pass.
	body := strings.TrimSpace(out)
	if i := strings.IndexAny(body, "[{"); i > 0 {
		body = body[i:]
	}
	if body == "" || body == "null" {
		return nil, nil
	}
	var pcs []Polecat
	if err := json.Unmarshal([]byte(body), &pcs); err != nil {
		return nil, fmt.Errorf("parsing polecat list for %s: %w", rig, err)
	}
	return pcs, nil
}

// Heartbeat reads one agent's heartbeat, or nil if it has not written one.
func (t Town) Heartbeat(rig Rig, polecat string) (*Heartbeat, error) {
	path := filepath.Join(t.Root, ".runtime", "heartbeats", rig.Prefix+"-"+polecat+".json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading heartbeat %s: %w", path, err)
	}
	var hb Heartbeat
	if err := json.Unmarshal(raw, &hb); err != nil {
		return nil, fmt.Errorf("parsing heartbeat %s: %w", path, err)
	}
	// A heartbeat with no timestamp cannot date anything. Treating it as
	// "written at the epoch" would report every such agent as stalled for
	// fifty-six years, so it is reported as absent instead.
	if hb.Timestamp.IsZero() {
		return nil, nil
	}
	return &hb, nil
}

// NukePolecat runs `gt polecat nuke <rig>/<name>`.
//
// This is the only destructive thing the witness does, and it is taken only
// where Gas Town has already returned SAFE_TO_NUKE.
func (t Town) NukePolecat(ctx context.Context, ref string) error {
	_, err := t.run(ctx, "polecat", "nuke", ref, "--force")
	return err
}

func (t Town) run(ctx context.Context, args ...string) (string, error) {
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, t.GT, args...)
	cmd.Dir = t.Root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("gt %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// EventsDir is where Gas Town writes the witness channel. Each file is one
// typed event — POLECAT_DONE and friends — dropped by the daemon and by the
// polecats themselves, which is why the channel keeps working with the stock
// Witness patrol switched off.
func (t Town) EventsDir() string {
	return filepath.Join(t.Root, "events", "witness")
}

// EventsFingerprint summarises the witness event channel cheaply.
//
// It is deliberately not a file watcher. inotify would mean a dependency and a
// descriptor per directory to answer a question a stat already answers, and the
// witness needs to survive the directory not existing yet — which it does not,
// until the first event is written.
func (t Town) EventsFingerprint() (count int, newest time.Time) {
	entries, err := os.ReadDir(t.EventsDir())
	if err != nil {
		return 0, time.Time{}
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".event") {
			continue
		}
		count++
		if info, err := e.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return count, newest
}
