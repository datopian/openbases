// Command wg-runner runs one agent against one bead, and stops.
//
// It replaces the Gas Town polecat lifecycle (ADR-0023). Gas Town could not
// start an agent on a headless node at all: it revives a process in a tmux pane
// that already exists and never creates the session, so on a machine nobody has
// sat at, every agent fails. What it was doing for us turned out to be a working
// directory, a settings file, a process with a deadline and a teardown.
//
// Run ON the execution node, as the cell's user, inside the cell's slice — so
// the CPU and memory limits from WP-I3 apply to the agent, not merely to this
// process. The caller is scripts/dispatch_bead.sh, which checks the budget
// first (ADR-0022) and refuses before anything is spent.
//
// One bead, one agent, one result. The merge queue and the pull request flow
// stay with Gas Town's refinery and WP-E3.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/datopian/workgraph/internal/config"
	"github.com/datopian/workgraph/internal/runner"
	"github.com/datopian/workgraph/internal/version"
)

func main() {
	var (
		bead     = flag.String("bead", "", "the bead to work")
		cell     = flag.String("cell", getenv("WG_CELL", ""), "execution cell slug")
		rig      = flag.String("rig", getenv("WG_RIG", "sandbox"), "rig within the cell")
		role     = flag.String("role", "polecat", "agent role, which decides the model tier")
		cellRoot = flag.String("cell-root", "", "the cell's home, e.g. /srv/cells/oss")
		instr    = flag.String("instructions", "", "what the agent is asked to do")
		model    = flag.String("model", "", "override the role's model tier")
		effort   = flag.String("effort", "", "override the role's reasoning effort")
		deadline = flag.Duration("deadline", 15*time.Minute, "how long the run may take")
		keep     = flag.Bool("keep", false, "leave the run directory behind for inspection")
		dryRun   = flag.Bool("dry-run", false, "plan and print, running nothing")
		jsonOut  = flag.Bool("json", false, "print the result as JSON")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if *cellRoot == "" && *cell != "" {
		*cellRoot = filepath.Join("/srv/cells", *cell)
	}
	// The agent reads its credentials from HOME. Set it here rather than
	// relying on the caller's environment, so a run started from anywhere finds
	// the cell's auth.
	if *cellRoot != "" {
		_ = os.Setenv("HOME", *cellRoot)
	}

	plan, err := runner.New(runner.Spec{
		Bead: *bead, Cell: *cell, Rig: *rig, Role: *role,
		CellRoot: *cellRoot, Instructions: *instr,
		GatewayToken: config.AIGatewayToken(),
		Deadline:     *deadline, Model: *model, Effort: *effort,
	})
	if err != nil {
		// Exit 2 for a bad request, distinct from a run that failed: a
		// dispatcher should be able to tell "I asked wrongly" from "the work
		// did not succeed".
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	if *dryRun {
		fmt.Printf("run dir   %s\nsettings  %s\nmodel     %s (effort %s)\ndeadline  %s\nmetadata  %s\n",
			plan.RunDir, plan.SettingsPath, plan.Model, plan.Effort, plan.Deadline, mustJSON(plan.Metadata))
		return
	}

	log.Info("starting", "build", version.String(), "bead", *bead, "cell", *cell, "dir", plan.RunDir)

	res := execute(log, plan, *keep)
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		fmt.Printf("%s: %s in %s\n", res.Bead, res.Status, res.Duration.Round(time.Second))
		if res.Output != "" {
			fmt.Println(res.Output)
		}
	}
	if res.Status != "completed" {
		os.Exit(1)
	}
}

// result is what a dispatcher needs to know afterwards.
type result struct {
	Bead     string        `json:"bead"`
	Status   string        `json:"status"` // completed | failed | timed_out
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration_ns"`
	Output   string        `json:"output,omitempty"`
	Error    string        `json:"error,omitempty"`
}

func execute(log *slog.Logger, plan runner.Plan, keep bool) result {
	res := result{Bead: filepath.Base(plan.RunDir), Status: "failed"}
	started := time.Now()

	// Teardown is registered before anything is created, and runs on every path
	// including a signal. dispatch_bead.sh learned this one the hard way: a
	// teardown that only runs on the happy path leaves agents running exactly
	// when something has gone wrong and nobody is watching.
	cleanup := func() {
		// The trust entry goes whether or not the directory is kept: it names a
		// path, and a file that accumulates one dead entry per run is a slow
		// leak in the cell's config.
		if err := trustWorkspace(plan.TrustFile, plan.RunDir, false); err != nil {
			log.Error("could not remove the workspace trust entry",
				"file", plan.TrustFile, "error", err)
		}
		// The settings file lives BESIDE the run directory now, so removing the
		// directory no longer takes it with it — and it holds the cell's
		// gateway token, so leaving it is worse than leaving a scratch dir.
		if err := os.Remove(plan.SettingsPath); err != nil && !os.IsNotExist(err) {
			log.Error("could not remove the settings file, which holds the gateway token",
				"file", plan.SettingsPath, "error", err)
		}
		if keep {
			log.Info("leaving the run directory behind", "dir", plan.RunDir)
			return
		}
		if err := os.RemoveAll(plan.RunDir); err != nil {
			log.Error("could not remove the run directory", "dir", plan.RunDir, "error", err)
		}
	}
	defer cleanup()

	// Both directories: the run directory the agent works in, and the one the
	// settings file sits beside it in.
	if err := os.MkdirAll(plan.RunDir, 0o700); err != nil {
		res.Error = err.Error()
		return res
	}
	if err := os.MkdirAll(filepath.Dir(plan.SettingsPath), 0o700); err != nil {
		res.Error = err.Error()
		return res
	}

	// Trust the workspace, or the permissions written below are ignored.
	//
	// The CLI refuses to honour a settings file in a workspace nobody has
	// accepted a trust dialog for, and there is nobody here to accept one. It
	// says so clearly and then asks for approval anyway, so the run completes
	// having done nothing and is billed for the asking.
	if err := trustWorkspace(plan.TrustFile, plan.RunDir, true); err != nil {
		// Not fatal. The run will ask for approvals it cannot get and do
		// nothing useful, but that is a better failure than refusing to start
		// and it is visible in the output.
		log.Error("could not mark the workspace trusted; the agent's permissions will be ignored",
			"file", plan.TrustFile, "error", err)
	}
	// 0600: the file holds the gateway token.
	if err := os.WriteFile(plan.SettingsPath, []byte(plan.Settings), 0o600); err != nil {
		res.Error = err.Error()
		return res
	}

	ctx, cancel := context.WithTimeout(context.Background(), plan.Deadline)
	defer cancel()
	// A signal has to reach the agent, not just this process, or a Ctrl-C on a
	// dispatch leaves the expensive half running.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := exec.CommandContext(ctx, plan.Argv[0], plan.Argv[1:]...)
	cmd.Dir = plan.RunDir
	// HOME stays the CELL, and the run directory is the working directory.
	//
	// The first version pointed HOME at the run directory, reasoning that
	// nothing the agent wrote should outlive the run. It also moved the agent
	// away from its credentials, and the run died with "Not logged in · Please
	// run /login". Auth and the cell-wide settings live in the cell's home;
	// per-run settings live in the working directory and take precedence there.
	// Both are needed, and they are not the same place.
	cmd.Env = os.Environ()
	// The gateway credential and the bead's attribution travel in the
	// environment, not in the settings file the agent reads (see runner.Plan).
	for k, v := range plan.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Kill the process GROUP on timeout. claude spawns children, and killing
	// only the parent leaves them holding the model connection — the exact
	// shape of the leak that made an idle town cost $10 an hour.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	out, err := cmd.CombinedOutput()
	res.Duration = time.Since(started)
	res.Output = string(out)

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Status = "timed_out"
		res.Error = fmt.Sprintf("the run exceeded its %s deadline and was killed", plan.Deadline)
		log.Error("run timed out", "bead", res.Bead, "deadline", plan.Deadline)
	case err != nil:
		res.Status = "failed"
		res.Error = err.Error()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		}
		log.Error("run failed", "bead", res.Bead, "error", err)
	default:
		res.Status = "completed"
		log.Info("run completed", "bead", res.Bead, "duration", res.Duration.Round(time.Second))
	}
	return res
}

// trustWorkspace adds or removes one entry in the CLI's config.
//
// Read-modify-write, which races if two dispatches run in the same cell at once.
// They do not today — dispatch is one bead at a time per cell — and the honest
// fix when they do is a lock file rather than pretending this is atomic.
func trustWorkspace(path, dir string, trusted bool) error {
	if path == "" {
		return nil
	}
	doc := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil {
			// A config we cannot parse is not ours to rewrite: replacing it
			// would drop whatever else the cell keeps in there.
			return fmt.Errorf("%s is not readable JSON: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	projects, _ := doc["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	if trusted {
		entry, _ := projects[dir].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		entry["hasTrustDialogAccepted"] = true
		projects[dir] = entry
	} else {
		delete(projects, dir)
	}
	doc["projects"] = projects

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
