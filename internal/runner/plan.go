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

	model := s.Model
	if model == "" {
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
	p.TrustFile = filepath.Join(s.CellRoot, ".claude.json")

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

	headers, err := renderHeaders(s.GatewayToken, p.Metadata)
	if err != nil {
		return Plan{}, err
	}

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

	settings, err := renderSettings(headers, tools, p.BeadsDir)
	if err != nil {
		return Plan{}, err
	}
	p.Settings = settings

	p.Model, p.Effort = model, effort
	p.Argv = []string{"claude", "-p", s.Instructions, "--model", model,
		"--settings", p.SettingsPath}
	if effort != "" {
		// --effort, not --reasoning-effort. The CLI rejects the latter with
		// "unknown option", which a run reports as a plain exit 1 — indis-
		// tinguishable from the agent failing at its actual work.
		p.Argv = append(p.Argv, "--effort", effort)
	}
	p.Deadline = deadline
	return p, nil
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
