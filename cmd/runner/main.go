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
	"sort"
	"strings"
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
		runtimeF = flag.String("runtime", "", "override the role's agent runtime (claude|opencode)")
		catPath  = flag.String("catalogue", getenv("WG_MODEL_CATALOGUE", "/etc/workgraph/models.json"),
			"role and model tables; absent means the built-in defaults")
		effort    = flag.String("effort", "", "override the role's reasoning effort")
		deadline  = flag.Duration("deadline", 15*time.Minute, "how long the run may take")
		maxAgents = flag.Int("max-agents", 0, "refuse if the cell already has this many runs (0 disables; wg-726)")
		keep      = flag.Bool("keep", false, "leave the run directory behind for inspection")
		dryRun    = flag.Bool("dry-run", false, "plan and print, running nothing")
		explain   = flag.Bool("explain", false, "print the exact argv, environment and directory, then run")
		jsonOut   = flag.Bool("json", false, "print the result as JSON")
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

	// Concurrency, before anything is created (wg-726).
	//
	// budget_limits has carried max_concurrent_agents since 0006 and nothing
	// ever read it: the cell slice bounds PROCESSES through TasksMax, which is
	// not the same thing and never was. The ceiling comes from the governing
	// budget and is passed in by the dispatcher, because the control plane knows
	// the limit and only the node can count what is running.
	if *maxAgents > 0 && *cellRoot != "" {
		running, cErr := runner.Concurrency(*cellRoot)
		if cErr != nil {
			log.Error("could not count running agents; refusing rather than exceeding the limit",
				"cell", *cell, "error", cErr)
			os.Exit(1)
		}
		if running >= *maxAgents {
			err := runner.ErrTooManyAgents{Cell: *cell, Running: running, Limit: *maxAgents}
			fmt.Fprintln(os.Stderr, err)
			// Exit 2: this is a refusal of the request, not a run that failed.
			os.Exit(2)
		}
	}

	// A missing catalogue is not an error — the built-in tables run. A present
	// one that is malformed IS, because that is somebody having configured
	// something and got it wrong, and running on defaults there is how a
	// deployment believes it changed a model and did not.
	catalogue, err := runner.LoadCatalogue(*catPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	plan, err := runner.New(runner.Spec{
		Bead: *bead, Cell: *cell, Rig: *rig, Role: *role,
		CellRoot: *cellRoot, Instructions: *instr,
		GatewayToken:   config.AIGatewayToken(),
		GatewayBaseURL: gatewayBaseURL(*cellRoot),
		Runtime:        runner.Runtime(*runtimeF),
		Catalogue:      catalogue,
		Deadline:       *deadline, Model: *model, Effort: *effort,
	})
	if err != nil {
		// Exit 2 for a bad request, distinct from a run that failed: a
		// dispatcher should be able to tell "I asked wrongly" from "the work
		// did not succeed".
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	if *dryRun {
		// The runtime is printed because it is now the thing that varies, and
		// because reading it back out of Argv by position is how the first
		// version of this printed the effort where the model should have been.
		effort := plan.Effort
		if _, ignored := plan.Env["WG_EFFORT_IGNORED"]; ignored {
			effort += " (ignored: this runtime has no equivalent)"
		}
		fmt.Printf("runtime   %s\nrun dir   %s\nsettings  %s\nmodel     %s (effort %s)\ndeadline  %s\nmetadata  %s\n",
			plan.Runtime, plan.RunDir, plan.SettingsPath, plan.Model, effort, plan.Deadline, mustJSON(plan.Metadata))
		return
	}

	log.Info("starting", "build", version.String(), "bead", *bead, "cell", *cell, "dir", plan.RunDir)

	// -explain exists because wg-azd was chased for hours by guessing at what
	// differed between this and a shell. Every candidate — the permission block,
	// the session store, XDG, the working directory, stdin, the process group —
	// was tested one at a time and worked. What was never done was reading the
	// exec itself, which is the one thing that could not be wrong.
	if *explain {
		explainExec(plan, config.AIGatewayToken())
	}

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
		// The state directory goes whether or not the run directory is kept: it
		// is the runtime's own scratch, it holds no evidence worth inspecting,
		// and leaving it is what wedges the next run.
		if plan.StatePath != "" {
			if err := os.RemoveAll(plan.StatePath); err != nil {
				log.Error("could not remove the runtime state directory",
					"dir", plan.StatePath, "error", err)
			}
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
	// Created, not just named. OpenCode is handed this as XDG_DATA_HOME and
	// will not start against a directory that is not there — which presents as
	// "Session not found", the same message a wedged shared store gives, so the
	// fix for one looks like it did not work for the other.
	if plan.StatePath != "" {
		if err := os.MkdirAll(plan.StatePath, 0o700); err != nil {
			log.Error("could not create the runtime state directory", "dir", plan.StatePath, "error", err)
			os.Exit(1)
		}
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
	// PWD is set to match, and OLDPWD dropped.
	//
	// cmd.Dir changes the child's working directory but not its idea of one:
	// PWD is an ordinary variable that a shell maintains, so a runner started
	// from a script hands the agent a PWD pointing wherever that script was.
	// OpenCode believes it over getcwd(), and against an unreadable /root it
	// exits immediately with "Session not found" — a message about neither
	// directories nor permissions, which is why wg-azd took eight ruled-out
	// hypotheses to find.
	//
	// It only appeared under a script. Run by hand through `sudo -u ... env`,
	// sudo strips PWD, the agent falls back to getcwd(), and everything works —
	// so every manual reproduction passed and the bake-off failed.
	//
	// An inherited PWD that disagrees with cmd.Dir is a lie to the child
	// whatever it does with it, so this is right regardless of OpenCode.
	cmd.Env = append(environWithout("PWD", "OLDPWD"), "PWD="+plan.RunDir)
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

// gatewayBaseURL reads the cell's own gateway endpoint out of its agent
// settings and strips the provider segment, leaving the prefix a runtime can
// append its own to.
//
// Read from the cell rather than configured separately, on exactly the argument
// scripts/dispatch_bead.sh makes about the token: a run must not reach a
// different gateway than the cell's own agents use. Two places to configure one
// endpoint is how they drift.
//
// Empty on any failure rather than an error, because only the opencode runtime
// needs it — and that runtime refuses without it, with a message that says so.
// Failing every claude run over a file it does not read would be worse.
func gatewayBaseURL(cellRoot string) string {
	if cellRoot == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(cellRoot, ".claude", "settings.json"))
	if err != nil {
		return ""
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	// .../<account>/<gateway>/anthropic -> .../<account>/<gateway>
	return strings.TrimSuffix(strings.TrimSuffix(doc.Env["ANTHROPIC_BASE_URL"], "/"), "/anthropic")
}

// explainExec prints exactly what the child is started with.
//
// The credential is redacted rather than omitted: knowing a variable is set, and
// how long its value is, is most of what a comparison needs, and printing the
// gateway token into a log would be a worse bug than the one being chased.
// environWithout is os.Environ() with the named variables removed, so a
// replacement can be appended without the child seeing both.
func environWithout(drop ...string) []string {
	skip := make(map[string]bool, len(drop))
	for _, d := range drop {
		skip[d] = true
	}
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !skip[k] {
			out = append(out, kv)
		}
	}
	return out
}

func explainExec(plan runner.Plan, secrets ...string) {
	fmt.Fprintf(os.Stderr, "exec  dir=%s\n", plan.RunDir)
	for i, a := range plan.Argv {
		fmt.Fprintf(os.Stderr, "exec  argv[%d]=%q\n", i, a)
	}
	keys := make([]string, 0, len(plan.Env))
	for k := range plan.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(os.Stderr, "exec  env %s=%s\n", k, redact(k, plan.Env[k], secrets))
	}
	// The whole inherited environment, not a chosen few. The child gets
	// os.Environ() plus the plan's additions, so anything here reaches it — and
	// "the exec looked identical" was true of the argv while the inherited
	// environment was never compared.
	inherited := os.Environ()
	sort.Strings(inherited)
	for _, kv := range inherited {
		k, v, _ := strings.Cut(kv, "=")
		fmt.Fprintf(os.Stderr, "exec  inherited %s=%s\n", k, redact(k, v, secrets))
	}
	fmt.Fprintf(os.Stderr, "exec  config=%s (%d bytes)\n", plan.SettingsPath, len(plan.Settings))
}

// redact hides a secret whether it is the whole value or buried in one.
//
// Redacting by variable NAME alone was not enough, and this printed a live
// gateway token to prove it: sudo publishes the command it ran as SUDO_COMMAND,
// so a token passed on a command line reappears inside an unrelated variable.
// The known secret values are matched wherever they occur.
func redact(key, value string, secrets []string) string {
	for _, s := range secrets {
		if s != "" && strings.Contains(value, s) {
			value = strings.ReplaceAll(value, s, fmt.Sprintf("<redacted, %d chars>", len(s)))
		}
	}
	lower := strings.ToLower(key)
	if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "key") {
		return fmt.Sprintf("<redacted, %d chars>", len(value))
	}
	return value
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
