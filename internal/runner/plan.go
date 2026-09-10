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
	"slices"
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
	// Catalogue overrides the built-in role and model tables. Nil uses them
	// (wg-3tp).
	Catalogue *Catalogue
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
	// RigDir is the rig holding the code the bead is about, which is not always
	// the rig the bead lives in.
	RigDir string
	// Env is what the run needs in its environment. Not the gateway credential,
	// which is only honoured from a settings file, and not anything secret.
	Env map[string]string
	// StatePath is a per-run directory for the runtime's own state, removed at
	// teardown. Empty when the runtime keeps no state worth isolating.
	//
	// OpenCode keeps sessions in a SQLite database under XDG_DATA_HOME. Shared
	// across runs it wedges: after one run in a cell, every later one fails with
	//
	//	Error: Session not found
	//
	// before reaching the model — which reads exactly like the model refusing to
	// use tools, and was recorded as four tool-use failures in wg-hvb before the
	// cause was found. The dispatcher runs job after job against one cell, so
	// shared state would have failed everything after the first.
	StatePath string
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

// DefaultModels maps a role to its model (ADR-0018, renamed by wg-8e0).
//
// Names are `<gateway-provider>/<model>` — the spelling the AI Gateway uses and
// the one that comes back in usage_records. One namespace, because there were
// three: this map said "sonnet", gastown_role_agents says "claude-sonnet", and
// the gateway logs "claude-sonnet-5". A model identifier that means different
// things in three files is how the wrong tier ships, and it ships quietly: the
// Claude Code CLI answers a name it does not know with
//
//	"claude-sonnet" is not a model this version of Claude Code recognizes
//
// on stderr, and then runs anyway on a default. A warning in a log nobody reads
// is indistinguishable from success.
//
// Each runtime translates to whatever its own CLI wants. The claude runtime
// strips the provider and REFUSES anything but anthropic, rather than passing a
// name it cannot reach and letting the CLI fall back.
//
// Not renamed: gastown_role_agents in Ansible. Those values are Gas Town agent
// ALIASES, not model identifiers — "claude-haiku" there names a gt agent whose
// command happens to be claude. Making them look like our names would be a
// third meaning wearing the same clothes.
var DefaultModels = map[string]string{
	"polecat": "anthropic/claude-sonnet-5",
	"crew":    "anthropic/claude-sonnet-5",
}

// splitModel separates the gateway provider from the model.
//
// Required rather than optional, because a bare name is exactly the ambiguity
// this rename removes: "claude-sonnet-5" could reasonably be addressed through
// anthropic, bedrock or vertex, and those are different endpoints with different
// credentials.
func splitModel(canonical string) (provider, model string, err error) {
	i := strings.Index(canonical, "/")
	if i <= 0 || i == len(canonical)-1 {
		return "", "", fmt.Errorf("model %q must be <provider>/<model>, e.g. anthropic/claude-sonnet-5", canonical)
	}
	return canonical[:i], canonical[i+1:], nil
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
// Context figures are Cloudflare's published ones, read from
// developers.cloudflare.com/workers-ai/models/<slug> on 2026-08-28. Three were
// wrong on first writing, all too small — deepseek by a factor of ten from a
// dropped digit — and a wrong-low context here does not fail loudly. It caps
// what the agent can see, so the run just does less well.
//
// A short table here rather than a lookup, because it is consulted while
// planning and a planner that makes a network call has a new failure mode. It
// moves to deployed configuration in wg-3tp; until then, adding a model is a
// pull request, which for something that decides what executes against our
// repositories is the right amount of friction.
var GatewayModels = map[string]ModelLimits{
	"anthropic/claude-sonnet-5":                         {Context: 200000, Output: 64000},
	"anthropic/claude-haiku-4-5":                        {Context: 200000, Output: 64000},
	"workers-ai/@cf/moonshotai/kimi-k2.7-code":          {Context: 262144, Output: 8192},
	"workers-ai/@cf/moonshotai/kimi-k2.6":               {Context: 262144, Output: 8192},
	"workers-ai/@cf/moonshotai/kimi-k3":                 {Context: 262144, Output: 8192},
	"workers-ai/@cf/deepseek-ai/deepseek-v4-flash-0731": {Context: 1310720, Output: 8192},
	"workers-ai/@cf/zai-org/glm-5.3-flash":              {Context: 1310720, Output: 8192},
	"workers-ai/@cf/google/gemma-4-26b-a4b-it":          {Context: 256000, Output: 8192},
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
		// A shell. Decided 2026-09-09, and the reasoning is worth keeping
		// because the previous line said the opposite for good reasons.
		//
		// What it replaced was `Bash(bd:*)` and `Bash(wg-browse:*)`: bd and a
		// browser and nothing else, on the argument that an agent with a shell
		// has `git push`, `curl` and `rm -rf`. That argument was sound when
		// written and it is not what the isolation actually rests on.
		//
		// The cell is the boundary (ADR-0002): its own Linux user and home,
		// cgroup limits on CPU, memory and processes, a credential profile,
		// and no way to read another cell's files. The tool allowlist sat on
		// top of that as a second belt.
		//
		// The two things the allowlist was really protecting turn out to be
		// protected elsewhere, which is what made this safe rather than
		// merely desirable:
		//
		//   Cloud instance metadata. nftables drops it per cell UID --
		//   `meta skuid 999 ip daddr 169.254.169.254 drop`, with eleven
		//   packets already caught on staging -- so it is refused whatever
		//   command asks. wg-browse's own check is defence in depth, not the
		//   only line.
		//
		//   The gateway credential. It is written into the agent's own
		//   opencode config as a request header, in the run directory the
		//   agent owns, so `Read` alone has always been enough to see it. A
		//   shell adds nothing there. Worth knowing rather than believing the
		//   older comment, which said the agent could not read it back.
		//
		// And what it was costing: sa-iyu was asked to prepare a dataset pack,
		// had no shell, could not run `ls`, globbed an empty run directory,
		// fell back to `bd list`, and exited 0 after 5m45s having done
		// nothing. Four msf beads read `blocked` for the same reason. An agent
		// that cannot run a build cannot scaffold a portal, and no amount of
		// Edit and Write substitutes for `npm install`.
		//
		// Landing stays with the landing path, and that is a division of
		// labour rather than a restriction: it snapshots the tree before the
		// run so only this run's work is committed, pushes with
		// --force-with-lease, and the pull request is opened by the control
		// API which holds the App key. An agent doing its own git would be a
		// second mechanism for the same thing, and two mechanisms for one job
		// is how they come to disagree. The instructions say so; with a shell
		// that is guidance rather than a wall, and it is stated as such.
		"Bash",
	},
	// Unchanged. Crew work is review and coordination rather than building,
	// so the case for a shell has not been made for it -- and granting one
	// "for symmetry" is how a boundary widens without a reason.
	"crew": {"Read", "Grep", "Glob", "Edit", "Write", "Bash(bd:*)", "Bash(wg-browse:*)"},
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
	if _, ok := s.Catalogue.runtimeFor(role); !ok {
		return p, fmt.Errorf("unknown role %q (known: %s)", role, s.Catalogue.knownRolesIn())
	}

	runtime := s.Runtime
	if runtime == "" {
		runtime, _ = s.Catalogue.runtimeFor(role)
	}
	if runtime != RuntimeClaude && runtime != RuntimeOpenCode {
		return p, fmt.Errorf("unknown runtime %q; expected %q or %q", runtime, RuntimeClaude, RuntimeOpenCode)
	}

	model := s.Model
	if model == "" {
		// The catalogue first, then the built-in table. A role configured onto a
		// gateway model must satisfy the opencode runtime the same way an
		// explicit -model does — refusing before looking here made a configured
		// model unusable, which is the whole point of wg-3tp.
		var ok bool
		if model, ok = s.Catalogue.modelFor(role); !ok {
			// Not defaulted to something cheap and not to something capable:
			// either would be a guess about cost, and the roles this runner
			// starts are a short, known list.
			return p, fmt.Errorf("no model tier for role %q (known: %s)", role, known(DefaultModels))
		}
	}
	// Checked after the lookup rather than before it, so a catalogue can supply
	// what the runtime needs. Claude Code's spellings are not the gateway's, and
	// handing one to /compat fails a long way from here.
	if runtime == RuntimeOpenCode && strings.HasPrefix(model, "anthropic/") {
		return p, fmt.Errorf("the %s runtime cannot reach %q through the gateway's Anthropic path; "+
			"give this role an explicit model, e.g. %s", RuntimeOpenCode, model,
			"workers-ai/@cf/moonshotai/kimi-k2.7-code")
	}

	effort := s.Effort
	if effort == "" {
		effort = s.Catalogue.effortFor(role) // absent is fine; the CLI has its own default
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

	// The graph that holds THIS BEAD, which is not always the rig the work runs
	// in.
	//
	// Without a graph at all the run is a no-op that costs money: the first
	// agent to get this far reported "I can't read wg-8el-trust's contents, so
	// I can't act on it, and I can't leave a comment on it either".
	//
	// And with the WRONG graph it is the same no-op, which is what routing by
	// repository introduced. A bead graph is per-rig: each rig has its own Dolt
	// database and its own id prefix, and one cannot see another's. sa-4yn was
	// filed in the sandbox rig and is about PortalJS, so it now runs in the
	// portaljs rig -- where `bd show sa-4yn` answers
	//
	//	Error fetching sa-4yn: no issue found matching "sa-4yn"
	//
	// The working tree has to be the rig that holds the code; the graph has to
	// be the rig that holds the bead. They were the same thing only while
	// every bead lived in one rig.
	if rig := strings.TrimSpace(s.Rig); rig != "" {
		p.RigDir = filepath.Join(s.CellRoot, "town", rig)
		p.BeadsDir = p.RigDir
		if owner := rigOwningBead(s.CellRoot, s.Bead); owner != "" && owner != rig {
			p.BeadsDir = filepath.Join(s.CellRoot, "town", owner)
		}
	}

	p.Env = map[string]string{}

	// The cell's PortalJS Arc credential, when it has one.
	//
	// Put in the environment rather than left in the file for the agent to
	// find, because every CLI that talks to Arc already reads PORTALJS_TOKEN
	// from there -- and an agent that has to be told to source a file is an
	// agent that will sometimes not.
	//
	// No weaker than the file it comes from: that file is 0600 and owned by
	// the cell user, which is the agent, so this is ergonomics rather than a
	// new grant. It is deliberately the ONLY deploy credential a cell gets --
	// the Cloudflare token terraform uses can edit DNS, Access policies and
	// every bucket, and is kept away from agent-run code.
	for k, v := range deployCredentials(s.CellRoot) {
		p.Env[k] = v
	}

	if p.BeadsDir != "" {
		// bd reads this from the environment, and it is not a secret.
		p.Env["BEADS_DIR"] = filepath.Join(p.BeadsDir, ".beads")
	}

	// What the agent is asked to do, plus -- when the two differ -- where its
	// bead actually lives.
	//
	// Two of sa-7dc's three runs spent themselves on this. The agent works in
	// the msf rig, whose .beads/config.yaml advertises the prefix `msf8`,
	// while its bead is `sa-7dc` and lives in another rig's graph, reached
	// through BEADS_DIR. `bd show sa-7dc` works. But an agent that reads the
	// local config and sees a prefix its bead does not match concludes it is
	// in the wrong place and goes looking, and the transcripts are exactly
	// that: one ended on a `bd list` dump of the whole graph, and the next
	// spent thirty minutes running `find` for .beads directories across every
	// rig in the town and wrote no files at all.
	//
	// Nothing was broken. The agent was reasoning correctly from what it could
	// see, and what it could see did not include the one fact that resolves
	// it. So the fact is stated. This is the same lesson as sa-kfh, which
	// spent forty calls discovering it could not push: an environment the
	// instructions do not describe gets investigated, and investigation is
	// the whole budget.
	// Whether this run can deploy, said either way.
	//
	// sa-fj3 spent a run finding out that it could not: it probed Arc, got a
	// device-flow code that needs a human to click "Authorize this device",
	// and correctly wrote the finding into the bead. That is a good report and
	// an expensive way to learn a fact the node already knew.
	if _, ok := p.Env["PORTALJS_TOKEN"]; ok {
		s.Instructions += "\n\nThis cell can deploy to PortalJS Arc: PORTALJS_TOKEN is " +
			"already in your environment, so the Arc CLI and API calls authenticate " +
			"without a login. Do not start a device flow -- it waits for a human to " +
			"click and there is nobody here to click it."
	} else {
		s.Instructions += "\n\nThis cell has NO PortalJS Arc credential, so a live " +
			"deploy is not possible from this run. Do not attempt the device flow: it " +
			"issues a code that needs a human to click \"Authorize this device\", and " +
			"the code expires before anyone will. Do everything that does not need the " +
			"deploy -- build, export, verify locally, write the runbook -- and say in " +
			"the bead that the deploy is waiting on a token."
	}

	// Folded into the Spec the two planners receive, which is passed by value,
	// so each runtime's argv carries it without either having to remember to.
	if p.BeadsDir != "" && p.RigDir != "" && p.BeadsDir != p.RigDir {
		s.Instructions += fmt.Sprintf(
			"\n\nOne thing about this checkout, so you do not have to work it out: "+
				"the bead %s lives in the graph at %s, which is already set as "+
				"BEADS_DIR, so plain `bd show %s` and `bd close %s` work from "+
				"anywhere. The .beads directory beside the code here belongs to a "+
				"different graph and advertises a different id prefix -- that is "+
				"expected, not a misconfiguration, and there is nothing to fix. Do "+
				"not go looking for your bead in other rigs.",
			s.Bead, filepath.Join(p.BeadsDir, ".beads"), s.Bead, s.Bead)
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
	settings, err := renderSettings(headers, tools, p.RigDir, p.BeadsDir)
	if err != nil {
		return err
	}
	p.Settings = settings

	// The CLI speaks Anthropic's names, so the provider segment comes off. It is
	// checked rather than assumed: Claude Code reaches the gateway's /anthropic
	// path and cannot address any other provider, so a Kimi model handed to this
	// runtime would produce a warning and a run on the wrong model.
	provider, model, err := splitModel(p.Model)
	if err != nil {
		return err
	}
	if provider != "anthropic" {
		return fmt.Errorf("the %s runtime reaches Anthropic models only; %q needs the %s runtime",
			RuntimeClaude, p.Model, RuntimeOpenCode)
	}

	p.Argv = []string{"claude", "-p", s.Instructions, "--model", model,
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
	limits, ok := s.Catalogue.limitsFor(p.Model)
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
	// A state directory of its own, beside the run rather than inside it, for
	// the same reason the config is: the agent has no business reading either.
	p.StatePath = filepath.Join(s.CellRoot, "runs", "."+s.Bead+".state")
	p.Env["XDG_DATA_HOME"] = p.StatePath
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
		// Derived from the same allowlist, so the two runtimes cannot drift.
		// This was written out by hand as {"*": "deny", "bd *": "allow"},
		// which meant adding a command to DefaultTools granted it under the
		// claude runtime and silently not under opencode — and opencode is the
		// default now, so the drift would have been the live behaviour: an
		// agent told it has a browser, with no permission to run it.
		"bash": bashPermissions(tools),
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

// deniedCommands are refused to every role, in both runtimes, whatever the
// allowlist says.
//
// It stops the ACCIDENT, not the adversary. Since 2026-09-09 the polecat role
// holds a real shell, and with a shell every one of these is reachable through
// a script, an alias or python -c. Denying the spelling stops an agent that
// helpfully tries to commit its own work; it stops nothing that is trying.
// Said plainly, because a deny list that reads like a boundary and is not one
// is worse than none: the boundary is the cell (a Linux user with its own uid
// and cgroups), the per-uid nftables rules that drop cloud metadata, and the
// gateway that scopes the token. This list is ergonomics on top of that.
//
// Landing is here as a division of labour rather than a fear: the landing path
// snapshots the tree before the run so only this run's work is committed,
// pushes with --force-with-lease, and opens the pull request through the
// control API, which holds the App key. Job.Instructions tells the agent so.
var deniedCommands = []string{"git push", "gh", "curl", "rm"}

// claudeDenies spells deniedCommands the way Claude Code's settings file wants.
func claudeDenies() []string {
	out := make([]string, 0, len(deniedCommands))
	for _, cmd := range deniedCommands {
		out = append(out, "Bash("+cmd+":*)")
	}
	return out
}

// bashPermissions turns the role's allowlist into OpenCode's bash block.
//
// OpenCode matches with the LAST pattern winning, so the catch-all deny goes in
// first and each allowed prefix after it. `Bash(bd:*)` becomes `bd *`.
func bashPermissions(tools []string) map[string]string {
	// A bare `Bash` is a shell, so the catch-all flips to allow and no prefix
	// list is built. Without this the role granting `Bash` produced
	// {"*": "deny"} -- an agent told it has a shell, with permission to run
	// nothing, which is the exact shape of the bug that started this: sa-iyu
	// had `Bash(bd:*)` and reported "The user has specified a rule which
	// prevents you from using this specific tool call" for `ls`.
	//
	// Checked before the prefix loop, and it wins outright: a role that grants
	// both `Bash` and `Bash(bd:*)` has granted a shell, and building a deny
	// list around it would be a fiction.
	if hasTool(tools, "Bash") {
		perm := map[string]string{"*": "allow"}
		// The same deny list Claude Code gets. Without this the two runtimes
		// disagreed about what a shell is, and which one an agent happened to
		// run under decided what it was allowed to do.
		//
		// Two patterns per command because OpenCode matches a glob against the
		// whole command line: `rm *` does not match a bare `rm`.
		for _, cmd := range deniedCommands {
			perm[cmd] = "deny"
			perm[cmd+" *"] = "deny"
		}
		return perm
	}
	perm := map[string]string{"*": "deny"}
	for _, tool := range tools {
		rest, ok := strings.CutPrefix(tool, "Bash(")
		if !ok {
			continue
		}
		rest = strings.TrimSuffix(rest, ")")
		// `bd:*` is Claude Code's spelling of "the bd command with any
		// arguments"; OpenCode wants `bd *`.
		rest = strings.TrimSuffix(rest, ":*")
		if rest == "" {
			continue
		}
		perm[rest+" *"] = "allow"
	}
	return perm
}

func hasTool(tools []string, want string) bool {
	for _, t := range tools {
		if t == want {
			return true
		}
	}
	return false
}

// joinSorted renders a name list stably, so an error message does not change
// between runs for no reason.
func joinSorted(names []string) string {
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
func renderSettings(headers string, tools []string, rigDir, beadsDir string) (string, error) {
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
			// Built from deniedCommands so this list and OpenCode's cannot
			// drift. Named explicitly even though nothing grants them: a deny
			// list that states the things an agent must never do survives
			// somebody widening the allow list without thinking about it.
			"deny": claudeDenies(),
			// BOTH rigs: the one holding the code, and the one holding the
			// bead. The sandbox otherwise confines the agent to its own run
			// directory, and it needs to read both.
			//
			// They are usually the same directory and were assumed to be until
			// routing by repository separated them. Giving only one is a run
			// that costs full price and cannot work: with only the bead's rig,
			// sa-4yn ran against PortalJS for 32.4 cents and reported "no
			// PortalJS source code accessible anywhere in this environment",
			// having searched town/sandbox -- the graph it could reach --
			// while town/portaljs sat outside its sandbox. With only the run's
			// rig it is the mirror image: the code is there and the bead
			// cannot be read or closed.
			"additionalDirectories": dirs(rigDir, beadsDir),
		},
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

// dirs returns a one-element list, or none, without a nil in the JSON.
// deployCredentials reads the cell's deploy credential file, if it has one.
//
// One file, one shape: KEY=value lines, as ansible writes it. Absent is the
// ordinary case and returns nothing -- a cell with no token is a cell that
// cannot deploy, which the instructions then say out loud rather than leaving
// the agent to discover through a 401.
//
// Only keys this function knows are passed on. A credential file that grows a
// line should not silently become part of every agent's environment.
func deployCredentials(cellRoot string) map[string]string {
	if strings.TrimSpace(cellRoot) == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(cellRoot, ".credentials", "portaljs.env"))
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if value == "" {
			continue
		}
		switch key {
		case "PORTALJS_TOKEN", "PORTALJS_API":
			out[key] = value
		}
	}
	return out
}

// rigOwningBead finds the rig whose graph a bead id belongs to, by its prefix.
//
// The prefix is what makes an id unambiguous across graphs, and every rig
// records its own in config.json, so this is a lookup rather than a guess:
//
//	sandbox   sa       portaljs   por7      datahub_next  datc
//
// An id whose prefix matches no rig returns empty, and the caller keeps the rig
// the work runs in. That is the safe direction and the old behaviour: a bead
// this cannot place is a bead that was already being looked for in the run's
// own graph.
func rigOwningBead(cellRoot, bead string) string {
	prefix, _, found := strings.Cut(strings.TrimSpace(bead), "-")
	if !found || prefix == "" {
		return ""
	}
	entries, err := os.ReadDir(filepath.Join(cellRoot, "town"))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(cellRoot, "town", e.Name(), "config.json"))
		if err != nil {
			continue
		}
		var config struct {
			Type  string `json:"type"`
			Beads struct {
				Prefix string `json:"prefix"`
			} `json:"beads"`
		}
		if err := json.Unmarshal(raw, &config); err != nil {
			continue
		}
		if config.Type == "rig" && config.Beads.Prefix == prefix {
			return e.Name()
		}
	}
	return ""
}

// dirs is the set of directories, in order, without duplicates or blanks.
//
// Deduplicated because the two rigs are the same directory in the ordinary
// case, and a settings file naming one path twice invites the reader to wonder
// which one is the real one.
func dirs(paths ...string) []string {
	out := []string{}
	for _, d := range paths {
		if d == "" || slices.Contains(out, d) {
			continue
		}
		out = append(out, d)
	}
	return out
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
