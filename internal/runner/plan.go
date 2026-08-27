// Package runner plans an agent run.
//
// ADR-0023 moved the agent lifecycle off Gas Town, which on a headless node
// could not start one at all. What Gas Town was doing that we now do ourselves
// is small: a working directory, a settings file, a process with a deadline,
// and a teardown. This package is the decision half of that — given a bead and a
// cell it produces the plan, and produces nothing else.
//
// Separated from the execution for the reason the witness, the monitor and the
// cost mapping are: every decision here has an expensive wrong answer, and each
// is trivial as a table-driven test and awkward to arrange against a live node.
// Where the settings file goes is the one that already cost us — a file two
// directories above where the agent runs is never read, which is why 98.3% of
// spend arrived untagged and why a polecat silently ran on the wrong model
// (wg-7yo).
package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Spec is what the caller knows.
type Spec struct {
	Bead string
	Cell string
	Rig  string
	// Role decides the model and the effort. Defaults to polecat, which is the
	// role that does the work; the supervisory roles belong to Gas Town and are
	// not started by this runner.
	Role string
	// CellRoot is the cell's home, e.g. /srv/cells/oss. Runs live beneath it so
	// they inherit the cell's ownership and its slice.
	CellRoot string
	// Instructions is what the agent is asked to do.
	Instructions string
	// GatewayToken authorises the request THROUGH the AI Gateway. Without it a
	// run either fails or, worse, reaches the provider directly — untagged,
	// unmetered and outside every budget. Required.
	GatewayToken string
	// Deadline bounds the run.
	Deadline time.Duration
	// GatewayBaseURL is the gateway prefix WITHOUT the provider segment, e.g.
	// https://gateway.ai.cloudflare.com/v1/<account>/workgraph-staging-oss.
	//
	// Required by the opencode runtime, which has to be told the whole endpoint;
	// unused by claude, which gets it from the cell's own settings. Read from
	// the cell rather than configured separately, on the same reasoning as the
	// token: a run must not reach a different gateway than the cell's own agents.
	GatewayBaseURL string
	// Runtime is which agent CLI runs this. Empty means the role's default.
	Runtime Runtime
	// Model and Effort override the role defaults when set.
	Model  string
	Effort string
	// AllowedTools is what the agent may do without asking. Empty means the
	// role's default.
	//
	// It has to be stated somewhere, because `claude -p` is non-interactive and
	// an agent with no permissions cannot act: the first end-to-end run finished
	// "successfully" having done nothing but ask
	//
	//   This requires user approval to run. Could you approve running
	//   `bd show wg-8el-e2e` so I can read the bead's contents?
	//
	// Nobody was there to answer, and the run was billed for the asking.
	AllowedTools []string
}

// Plan is what to do, with nothing left to decide.
type Plan struct {
	RunDir       string
	SettingsPath string
	Settings     string
	Argv         []string
	Deadline     time.Duration
	Metadata     map[string]string
	// Model and Effort are stated rather than left to be read back out of Argv
	// by position. The first version of the dry-run printed the effort where the
	// model should have been, because it took the last argument.
	Model  string
	Effort string
	// Runtime is which CLI Argv[0] is, stated rather than inferred from it.
	Runtime Runtime
	// AllowedTools is what the agent may do without being asked.
	AllowedTools []string
	// BeadsDir is the graph the agent reads and closes its bead in.
	BeadsDir string
	// Env is what the run needs in its environment. Not the gateway credential,
	// which is only honoured from a settings file, and not anything secret.
	Env map[string]string
	// TrustFile is the CLI's own config, where a workspace is marked trusted.
	//
	// Permissions in a settings file are IGNORED in an untrusted workspace, and
	// the run says so and then asks for approval anyway:
	//
	//   Ignoring 6 permissions.allow entries from .claude/settings.json: this
	//   workspace has not been trusted.
	//
	// Trusting it is not a judgement about the code in it — the runner created
	// the directory a moment earlier and put a single settings file in it. The
	// entry is removed at teardown, so the file does not accumulate one per run.
	TrustFile string
}

// DefaultModels maps a role to its model tier (ADR-0018).
//
// These are the names the Claude Code CLI accepts, which are NOT the names Gas
// Town uses. gastown_role_agents says "claude-sonnet"; the CLI answers that with
//
//	"claude-sonnet" is not a model this version of Claude Code recognizes
//
// and then runs anyway on a default, assuming a 200k context — so the wrong tier
// is a warning on stderr rather than a failure, and would have been easy to miss
// in a log nobody reads. The tiers are the same; only the spelling differs.
var DefaultModels = map[string]string{
	"polecat": "sonnet",
	"crew":    "sonnet",
}

// Runtime is which agent CLI executes the run.
//
// A property of the RUN, not of the deployment (ADR-0024). The role picks the
// default and a spec may override it, which is what makes trying another model
// on one bead a flag rather than a project.
type Runtime string

const (
	// RuntimeClaude is Claude Code, reaching Anthropic models through the
	// gateway's /anthropic path. The default, and unchanged by ADR-0024.
	RuntimeClaude Runtime = "claude"
	// RuntimeOpenCode is OpenCode, reaching any gateway provider through
	// /compat. Proven to carry attribution in wg-uhj.
	RuntimeOpenCode Runtime = "opencode"
)

// DefaultRuntimes maps a role to the CLI that runs it.
//
// Both on claude, deliberately. ADR-0024 makes another runtime possible; it
// does not migrate anything, because a runtime change is a change to what
// executes model output against our repositories and deserves a bead of its
// own.
var DefaultRuntimes = map[string]Runtime{
	"polecat": RuntimeClaude,
	"crew":    RuntimeClaude,
}

// ModelLimits is what a model can take, which OpenCode requires to be told.
//
// Not a nicety: without a limit OpenCode asks for max_tokens=32000, and a model
// that caps lower refuses the request before doing any work —
//
//	max_tokens=32000 cannot be greater than max_model_len=max_total_tokens=24000
//
// Context is also a hard filter on which models can be workers at all,
// separately from how good they are. OpenCode's agent system prompt is roughly
// 19,900 tokens, so a 24k model cannot host it: llama-3.3-70b is a capable T0
// classifier and an unusable worker, and the two facts are unrelated.
type ModelLimits struct {
	Context int
	Output  int
}

// GatewayModels is what we know about the models reachable through the gateway.
//
// A short table here rather than a lookup, because it is consulted while
// planning and a planner that makes a network call has a new failure mode. It
// moves to deployed configuration in wg-3tp; until then, adding a model is a
// pull request, which for something that decides what executes against our
// repositories is the right amount of friction.
var GatewayModels = map[string]ModelLimits{
	"workers-ai/@cf/moonshotai/kimi-k2.7-code":          {Context: 262144, Output: 8192},
	"workers-ai/@cf/moonshotai/kimi-k2.6":               {Context: 262144, Output: 8192},
	"workers-ai/@cf/moonshotai/kimi-k3":                 {Context: 262144, Output: 8192},
	"workers-ai/@cf/deepseek-ai/deepseek-v4-flash-0731": {Context: 131072, Output: 8192},
	"workers-ai/@cf/zai-org/glm-5.3-flash":              {Context: 131072, Output: 8192},
	"workers-ai/@cf/qwen/qwen3-30b-a3b-fp8":             {Context: 32768, Output: 4096},
}

// DefaultTools is what each role may do unattended.
//
// A deliberately short list, and short in a specific direction: the agent can
// READ widely and WRITE almost nothing. It can drive the bead it was given
// through bd, read and search the tree, and edit files in its own working
// directory. It cannot run arbitrary shell, and it cannot push.
//
// Plan §8.3 makes the allowed tools part of the signed capability, so this is
// the default rather than the authority — a capability that grants less should
// narrow it, and one that grants more is a decision somebody signed for.
var DefaultTools = map[string][]string{
	"polecat": {
		"Read", "Grep", "Glob", "Edit", "Write",
		// bd only, not Bash generally. `Bash(bd:*)` is a prefix match, so this
		// is the bead tool and nothing else — no `git push`, no `curl`, no
		// `rm -rf`, and no shelling out to reach any of them.
		"Bash(bd:*)",
	},
	"crew": {"Read", "Grep", "Glob", "Edit", "Write", "Bash(bd:*)"},
}

// DefaultEffort mirrors gastown_role_effort. Effort matters more than the model
// choice did: output was 89% of tokens and effectively all of the cost in the
// first bill.
var DefaultEffort = map[string]string{
	"polecat": "medium",
	"crew":    "medium",
}

const (
	// MaxDeadline bounds a single run. Not a policy about how long work may
	// take — the runtime ceiling timer is that — but a guard against a spec
	// asking for a run nothing will ever reap.
	MaxDeadline = 4 * time.Hour
	// MinDeadline is short enough for a probe and long enough that a real run
	// is not killed while it is still starting.
	MinDeadline = 30 * time.Second
)

// beadName is deliberately strict: the bead id becomes a directory name and
// reaches a shell, and a bead id is a short slug in every graph we have.
var beadName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// New plans a run, or explains why it cannot.
func New(s Spec) (Plan, error) {
	var p Plan

	if !beadName.MatchString(s.Bead) {
		return p, fmt.Errorf("bead %q is not a usable identifier", s.Bead)
	}
	if strings.TrimSpace(s.Cell) == "" {
		return p, fmt.Errorf("a run needs a cell")
	}
	if strings.TrimSpace(s.CellRoot) == "" {
		return p, fmt.Errorf("a run needs the cell root directory")
	}
	if !filepath.IsAbs(s.CellRoot) {
		return p, fmt.Errorf("the cell root %q must be absolute", s.CellRoot)
	}
	if strings.TrimSpace(s.Instructions) == "" {
		return p, fmt.Errorf("a run needs instructions; an agent with nothing to do still costs money")
	}
	if strings.TrimSpace(s.GatewayToken) == "" {
		// Refused rather than defaulted. Without the gateway the request still
		// succeeds — it just goes straight to the provider, where it is
		// untagged, unmetered and outside every budget. That is the one failure
		// mode this whole chain exists to prevent.
		return p, fmt.Errorf("a run needs the AI Gateway token, or its spend escapes every budget")
	}

	role := strings.TrimSpace(s.Role)
	if role == "" {
		role = "polecat"
	}

	// The role is checked once, here, against the runtime table — which is the
	// table every role must appear in. Three lookups each reporting their own
	// failure meant an unknown role was reported as a missing runtime, which
	// says nothing about the actual mistake.
	if _, ok := DefaultRuntimes[role]; !ok {
		return p, fmt.Errorf("unknown role %q (known: %s)", role, knownRoles())
	}

	runtime := s.Runtime
	if runtime == "" {
		runtime = DefaultRuntimes[role]
	}
	if runtime != RuntimeClaude && runtime != RuntimeOpenCode {
		return p, fmt.Errorf("unknown runtime %q; expected %q or %q", runtime, RuntimeClaude, RuntimeOpenCode)
	}

	model := s.Model
	if model == "" {
		if runtime == RuntimeOpenCode {
			// No default, and deliberately not one borrowed from DefaultModels:
			// those are Claude Code's spellings, and handing "sonnet" to the
			// gateway's /compat endpoint would fail somewhere far from here.
			// Naming models one way for every runtime is wg-8e0.
			return p, fmt.Errorf("the %s runtime needs an explicit model, e.g. %s",
				RuntimeOpenCode, "workers-ai/@cf/moonshotai/kimi-k2.7-code")
		}
		var ok bool
		if model, ok = DefaultModels[role]; !ok {
			// Not defaulted to something cheap and not to something capable:
			// either would be a guess about cost, and the roles this runner
			// starts are a short, known list.
			return p, fmt.Errorf("no model tier for role %q (known: %s)", role, known(DefaultModels))
		}
	}
	effort := s.Effort
	if effort == "" {
		effort = DefaultEffort[role] // absent is fine; the CLI has its own default
	}

	deadline := s.Deadline
	switch {
	case deadline == 0:
		return p, fmt.Errorf("a run needs a deadline; an agent nothing stops is the expensive failure")
	case deadline < MinDeadline:
		return p, fmt.Errorf("deadline %s is below the %s minimum", deadline, MinDeadline)
	case deadline > MaxDeadline:
		return p, fmt.Errorf("deadline %s is above the %s maximum", deadline, MaxDeadline)
	}

	// The run directory is derived from the bead so it is stable across a retry
	// and obvious to a person looking at the node. Under the cell root, so it
	// inherits the cell's ownership and its slice.
	p.RunDir = filepath.Join(s.CellRoot, "runs", s.Bead)

	// The settings file lives BESIDE the run directory, not inside it, and is
	// passed with --settings.
	//
	// Two constraints meet here and only this satisfies both.
	//
	// It has to be a settings file at all: the gateway headers are only honoured
	// from a settings `env` block. Putting them in the process environment
	// instead looks equivalent and is not — a run done that way reached the
	// gateway and arrived completely untagged, which is the same silent failure
	// wg-7yo spent a week on, reintroduced from the other direction.
	//
	// And it must not be readable by the agent, because it holds the cell's
	// gateway token. The first agent to run with the file in its working
	// directory found the credential and said so, declining to use it. It
	// behaved well; the next one might not.
	//
	// A sibling of the run directory is outside the agent's sandbox — which
	// covers its working directory and the rig, and nothing else — while still
	// being a path the CLI can be pointed at.
	p.SettingsPath = filepath.Join(s.CellRoot, "runs", "."+s.Bead+".settings.json")
	// Claude Code only. OpenCode has no trusted-workspace concept, and setting
	// this for it would have the teardown remove an entry nothing wrote.
	if runtime == RuntimeClaude {
		p.TrustFile = filepath.Join(s.CellRoot, ".claude.json")
	}

	p.Metadata = map[string]string{
		"role": role,
		"cell": s.Cell,
		"bead": s.Bead,
	}
	if rig := strings.TrimSpace(s.Rig); rig != "" {
		p.Metadata["rig"] = rig
	}

	tools := s.AllowedTools
	if len(tools) == 0 {
		var ok bool
		if tools, ok = DefaultTools[role]; !ok {
			return p, fmt.Errorf("no tool allowlist for role %q", role)
		}
	}
	p.AllowedTools = tools

	// The rig's graph, which is what the agent has to read and close.
	//
	// Without it the run is a no-op that costs money: the first agent to get
	// this far reported "I can't read wg-8el-trust's contents, so I can't act on
	// it, and I can't leave a comment on it either".
	if rig := strings.TrimSpace(s.Rig); rig != "" {
		p.BeadsDir = filepath.Join(s.CellRoot, "town", rig)
	}

	p.Env = map[string]string{}
	if p.BeadsDir != "" {
		// bd reads this from the environment, and it is not a secret.
		p.Env["BEADS_DIR"] = filepath.Join(p.BeadsDir, ".beads")
	}

	p.Model, p.Effort, p.Runtime = model, effort, runtime

	// From here the two runtimes diverge completely: different config file,
	// different way of carrying the credential, different argv. What they share
	// is above, and what they must both satisfy is that the credential ends up
	// somewhere the agent cannot read it.
	switch runtime {
	case RuntimeClaude:
		if err := planClaude(&p, s, tools); err != nil {
			return Plan{}, err
		}
	case RuntimeOpenCode:
		if err := planOpenCode(&p, s, tools); err != nil {
			return Plan{}, err
		}
	}

	p.Deadline = deadline
	return p, nil
}

// planClaude fills in the Claude Code half. Unchanged behaviour; only moved.
func planClaude(p *Plan, s Spec, tools []string) error {
	headers, err := renderHeaders(s.GatewayToken, p.Metadata)
	if err != nil {
		return err
	}
	settings, err := renderSettings(headers, tools, p.BeadsDir)
	if err != nil {
		return err
	}
	p.Settings = settings
	p.Argv = []string{"claude", "-p", s.Instructions, "--model", p.Model,
		"--settings", p.SettingsPath}
	if p.Effort != "" {
		// --effort, not --reasoning-effort. The CLI rejects the latter with
		// "unknown option", which a run reports as a plain exit 1 — indis-
		// tinguishable from the agent failing at its actual work.
		p.Argv = append(p.Argv, "--effort", p.Effort)
	}
	return nil
}

// planOpenCode fills in the OpenCode half (wg-uhj proved the shape).
//
// Three differences from Claude Code that are not cosmetic.
//
// The credential travels in a provider's options.headers rather than in an env
// block, and the config file is named by OPENCODE_CONFIG rather than --settings.
// The file still lives beside the run directory and not in it, for the same
// reason: an agent with Read in its own working directory can read its own
// gateway token, which has happened here once already.
//
// The model must carry its limits, because OpenCode otherwise asks for 32000
// output tokens and a smaller model refuses the whole request.
//
// And the permission model is the opposite way round. OpenCode allows
// everything by default, so the config denies "*" first and allows back the
// short list — where Claude Code takes an allowlist and we add a deny list on
// top of it. Same intent, expressed from the other end.
func planOpenCode(p *Plan, s Spec, tools []string) error {
	base := strings.TrimSuffix(strings.TrimSpace(s.GatewayBaseURL), "/")
	if base == "" {
		return fmt.Errorf("the %s runtime needs the gateway base URL", RuntimeOpenCode)
	}
	limits, ok := GatewayModels[p.Model]
	if !ok {
		return fmt.Errorf("no limits known for model %q; add it to GatewayModels, "+
			"because without them OpenCode requests 32000 output tokens and the model refuses", p.Model)
	}

	cfg, err := renderOpenCodeConfig(base, s.GatewayToken, p.Model, limits, p.Metadata, tools, s.CellRoot)
	if err != nil {
		return err
	}
	p.Settings = cfg
	// The provider id is ours, so the model reaching the CLI is
	// <our-provider>/<gateway-provider>/<model>.
	p.Argv = []string{"opencode", "run", "-m", openCodeProvider + "/" + p.Model, s.Instructions}
	// Not --auto. Every permission below is an explicit allow or deny, so
	// nothing is left to ask; --auto would additionally approve anything a
	// future OpenCode version adds that we have not thought about.
	p.Env["OPENCODE_CONFIG"] = p.SettingsPath
	// Effort has no equivalent here. Saying so is better than silently dropping
	// it, because a run that ignored the tier would look identical to one that
	// honoured it.
	if p.Effort != "" {
		p.Env["WG_EFFORT_IGNORED"] = p.Effort
	}
	return nil
}

// openCodeProvider is the id of the provider block the runner writes. Ours, not
// one of OpenCode's built-ins, because it points at OUR gateway with OUR
// attribution headers and must not be confused with a stock cloudflare entry.
const openCodeProvider = "wg-gateway"

// renderOpenCodeConfig builds the config file OPENCODE_CONFIG points at.
//
// The permission block is the security boundary and is written from deny-all
// upwards. OpenCode evaluates patterns with the LAST match winning, so the
// catch-all comes first and the specific allows after it — the opposite order
// reads the same and denies everything.
func renderOpenCodeConfig(base, token, model string, limits ModelLimits,
	metadata map[string]string, tools []string, cellRoot string) (string, error) {

	if len(metadata) > 5 {
		return "", fmt.Errorf("%d metadata keys exceeds the gateway's limit of 5", len(metadata))
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}

	// Translated from the same role allowlist the claude runtime uses, so the
	// two runtimes grant the same thing and a reader can compare them. The
	// Bash(bd:*) entry is the one that matters: bd and nothing else.
	perm := map[string]any{
		"*":     "deny",
		"read":  "allow",
		"glob":  "allow",
		"grep":  "allow",
		"edit":  "allow",
		"write": "allow",
		"bash": map[string]string{
			"*":    "deny",
			"bd *": "allow",
		},
		// Everything outside the run directory and the rig is denied. Claude
		// Code has no equivalent of this and relies on the sandbox; here it is
		// stated, which is strictly better.
		"external_directory": map[string]string{
			"*":                                     "deny",
			filepath.Join(cellRoot, "town") + "/**": "allow",
		},
	}
	if !hasTool(tools, "Write") {
		perm["write"] = "deny"
	}
	if !hasTool(tools, "Edit") {
		perm["edit"] = "deny"
	}

	doc := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{
			openCodeProvider: map[string]any{
				// The OpenAI-compatible adapter, against the gateway's /compat
				// endpoint. That is where dynamic routes live too, so the same
				// block reaches them later without changing shape.
				"npm":  "@ai-sdk/openai-compatible",
				"name": "Workgraph gateway",
				"options": map[string]any{
					"baseURL": base + "/compat",
					"headers": map[string]string{
						"cf-aig-authorization": "Bearer " + token,
						"cf-aig-metadata":      string(encoded),
					},
				},
				"models": map[string]any{
					model: map[string]any{
						"name":  model,
						"limit": map[string]int{"context": limits.Context, "output": limits.Output},
					},
				},
			},
		},
		"permission": perm,
		// Off, because an agent that updates itself mid-run is a different
		// binary than the one versions.lock pinned.
		"autoupdate": false,
		// Nothing leaves the node. The default is off, and stating it means a
		// changed default does not quietly start publishing run transcripts.
		"share": "disabled",
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

func hasTool(tools []string, want string) bool {
	for _, t := range tools {
		if t == want {
			return true
		}
	}
	return false
}

// knownRoles lists the roles this runner starts. DefaultRuntimes is the
// authority, because every role needs one and a role missing from it cannot run
// at all — whereas a role can legitimately take its model from a spec.
func knownRoles() string {
	names := make([]string, 0, len(DefaultRuntimes))
	for r := range DefaultRuntimes {
		names = append(names, r)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func knownRuntimes() string {
	return string(RuntimeClaude) + ", " + string(RuntimeOpenCode)
}

// renderHeaders builds the gateway header value: the credential and the
// attribution, which have to travel together because they are one variable.
//
// A project settings file REPLACES the whole value rather than merging it, so
// the authorization line cannot be left in the cell's settings and the metadata
// added here — that was the trap wg-set-agent-metadata already documented, and
// it is why both end up in the environment together.
func renderHeaders(token string, metadata map[string]string) (string, error) {
	// Five keys is the AI Gateway maximum and extras are dropped SILENTLY, so a
	// sixth would not fail — it would quietly stop one of these arriving.
	if len(metadata) > 5 {
		return "", fmt.Errorf("%d metadata keys exceeds the gateway's limit of 5", len(metadata))
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	return "cf-aig-authorization: Bearer " + token + "\n" +
		"cf-aig-metadata: " + string(encoded), nil
}

// renderSettings builds the settings file the CLI is pointed at.
func renderSettings(headers string, tools []string, beadsDir string) (string, error) {
	doc := map[string]any{
		// Honoured only from here. The same value exported into the process
		// environment is ignored, and the run then reaches the gateway with no
		// attribution at all.
		"env": map[string]string{"ANTHROPIC_CUSTOM_HEADERS": headers},
		// In the settings file rather than on the command line, because this is
		// the security boundary and a file in the run directory is reviewable
		// after the fact — an argv is gone when the process is.
		"permissions": map[string]any{
			"allow": tools,
			// Named explicitly even though nothing grants them. A deny list that
			// states the things an agent must never do survives somebody
			// widening the allow list without thinking about it.
			"deny": []string{
				"Bash(git push:*)",
				"Bash(gh:*)",
				"Bash(curl:*)",
				"Bash(rm:*)",
			},
			// The rig, so the agent can reach the graph its bead lives in. The
			// sandbox otherwise confines it to its own working directory, and an
			// agent that cannot read its bead cannot do anything with it.
			"additionalDirectories": dirs(beadsDir),
		},
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

// dirs returns a one-element list, or none, without a nil in the JSON.
func dirs(d string) []string {
	if d == "" {
		return []string{}
	}
	return []string{d}
}

func known(m map[string]string) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// Concurrency reports how many runs are live in a cell.
//
// Counted from the run directories, which the runner creates before it starts
// an agent and removes on every teardown path. Counting processes instead would
// depend on what the agent binary happens to be called; counting directories
// depends only on this package's own behaviour.
//
// It races two dispatches starting at the same instant, which does not happen
// today because dispatch is one bead at a time per cell. The honest fix when it
// does is a lock, not a more careful count, and this is the place it would go.
func Concurrency(cellRoot string) (int, error) {
	entries, err := os.ReadDir(filepath.Join(cellRoot, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		// Directories only. The settings file for a live run is a sibling of
		// these, and counting it would double every run.
		if e.IsDir() {
			n++
		}
	}
	return n, nil
}

// ErrTooManyAgents reports a cell already at its concurrent-agent ceiling.
type ErrTooManyAgents struct {
	Cell    string
	Running int
	Limit   int
}

func (e ErrTooManyAgents) Error() string {
	return fmt.Sprintf("cell %s already has %d agent(s) running and its budget allows %d",
		e.Cell, e.Running, e.Limit)
}
