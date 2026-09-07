package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func spec() Spec {
	return Spec{
		Bead: "wg-qw1", Cell: "oss", Rig: "sandbox",
		CellRoot: "/srv/cells/oss", Instructions: "close this bead",
		GatewayToken: "tok", Deadline: 10 * time.Minute,
	}
}

// Two constraints meet on the settings file and only one arrangement satisfies
// both. It has to BE a settings file, because the gateway headers are honoured
// only from a settings env block — putting them in the process environment
// instead reaches the gateway completely untagged. And it must not sit where the
// agent can read it, because it holds the cell's gateway token.
//
// Beside the run directory, passed with --settings: applied by the CLI,
// unreachable by a sandbox that covers the working directory and the rig.
func TestTheSettingsFileIsAppliedButNotReadable(t *testing.T) {
	p, err := New(spec())
	if err != nil {
		t.Fatal(err)
	}

	if strings.HasPrefix(p.SettingsPath, p.RunDir+"/") {
		t.Fatalf("the settings file is inside the agent's working directory, so it can read the token: %s",
			p.SettingsPath)
	}
	argv := strings.Join(p.Argv, " ")
	if !strings.Contains(argv, "--settings "+p.SettingsPath) {
		t.Fatalf("the CLI is not pointed at the settings file, so none of it applies: %s", argv)
	}
}

func TestTheRunCarriesItsBeadIntoTheGatewayLog(t *testing.T) {
	p, err := New(spec())
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(p.Settings), &doc); err != nil {
		t.Fatalf("the settings file is not valid JSON: %v", err)
	}
	headers := doc.Env["ANTHROPIC_CUSTOM_HEADERS"]

	// In the settings env block, NOT in p.Env. The same value exported into the
	// process environment is ignored by the CLI, and a run done that way arrives
	// at the gateway with no attribution — verified by doing it.
	if _, leaked := p.Env["ANTHROPIC_CUSTOM_HEADERS"]; leaked {
		t.Error("the headers are in the process environment, where the CLI ignores them")
	}
	if !strings.Contains(headers, "cf-aig-authorization: Bearer tok") {
		t.Error("the gateway authorization header is missing, so the request would not reach the gateway")
	}
	line := ""
	for _, l := range strings.Split(headers, "\n") {
		if strings.HasPrefix(l, "cf-aig-metadata: ") {
			line = strings.TrimPrefix(l, "cf-aig-metadata: ")
		}
	}
	if line == "" {
		t.Fatal("no cf-aig-metadata, so spend could not be attributed to anything")
	}
	var md map[string]string
	if err := json.Unmarshal([]byte(line), &md); err != nil {
		t.Fatalf("the metadata is not valid JSON: %v", err)
	}
	for k, want := range map[string]string{"bead": "wg-qw1", "cell": "oss", "rig": "sandbox", "role": "polecat"} {
		if md[k] != want {
			t.Errorf("metadata %s = %q, want %q", k, md[k], want)
		}
	}
}

// An agent that cannot read its bead cannot do anything with it. The first run
// to get past the permission problem reported exactly that: "I can't read
// wg-8el-trust's contents, so I can't act on it".
func TestTheAgentCanReachTheGraphItsBeadLivesIn(t *testing.T) {
	p, err := New(spec())
	if err != nil {
		t.Fatal(err)
	}
	if p.BeadsDir != "/srv/cells/oss/town/sandbox" {
		t.Fatalf("beads dir is %q", p.BeadsDir)
	}
	if p.Env["BEADS_DIR"] != "/srv/cells/oss/town/sandbox/.beads" {
		t.Errorf("BEADS_DIR is %q, so bd would look in the wrong place", p.Env["BEADS_DIR"])
	}
	if !strings.Contains(p.Settings, "additionalDirectories") ||
		!strings.Contains(p.Settings, "/srv/cells/oss/town/sandbox") {
		t.Error("the rig is not in additionalDirectories, so the sandbox confines the agent away from it")
	}
	// And the sandbox must NOT be widened to the cell root, which is where the
	// settings file and the credentials live.
	if strings.Contains(p.Settings, `"/srv/cells/oss"`) {
		t.Error("the whole cell is in additionalDirectories, which exposes the settings file and the credentials")
	}
}

// Five keys is the gateway's maximum and extras are dropped SILENTLY, so a sixth
// would not fail — it would quietly stop one of the others arriving.
func TestMetadataStaysWithinTheGatewayLimit(t *testing.T) {
	p, err := New(spec())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Metadata) > 5 {
		t.Fatalf("%d metadata keys; the gateway keeps 5 and drops the rest without saying so", len(p.Metadata))
	}
}

// Without the gateway token the run still WORKS — it goes straight to the
// provider, untagged, unmetered and outside every budget. That is the one
// outcome the whole cost chain exists to prevent, so it is refused.
func TestARunWithoutTheGatewayTokenIsRefused(t *testing.T) {
	s := spec()
	s.GatewayToken = ""
	_, err := New(s)
	if err == nil {
		t.Fatal("a run with no gateway token was planned")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("the refusal should say what is at stake: %v", err)
	}
}

func TestARunMustBeStoppable(t *testing.T) {
	s := spec()
	s.Deadline = 0
	if _, err := New(s); err == nil {
		t.Fatal("a run with no deadline was planned")
	}

	s.Deadline = 10 * time.Second
	if _, err := New(s); err == nil {
		t.Fatal("a deadline below the minimum was accepted")
	}

	s.Deadline = 99 * time.Hour
	if _, err := New(s); err == nil {
		t.Fatal("a deadline above the maximum was accepted")
	}
}

// An unknown role must not silently get a model. Defaulting to something cheap
// makes the work bad; defaulting to something capable makes the bill bad; both
// are guesses the caller should not be allowed to make by omission.
func TestAnUnknownRoleIsRefusedRatherThanGuessed(t *testing.T) {
	s := spec()
	s.Role = "archduke"
	_, err := New(s)
	if err == nil {
		t.Fatal("an unknown role was given a model")
	}
	if !strings.Contains(err.Error(), "polecat") {
		t.Errorf("the error should list the roles that exist: %v", err)
	}
}

func TestTheRoleDecidesTheModelAndTheEffort(t *testing.T) {
	p, err := New(spec())
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(p.Argv, " ")
	// The provider segment comes off for the CLI, which speaks Anthropic's
	// names; the canonical form keeps it (wg-8e0).
	if !strings.Contains(argv, "--model claude-sonnet-5") {
		t.Errorf("polecat should run on its tier: %s", argv)
	}
	if strings.Contains(argv, "--model anthropic/") {
		t.Errorf("the provider segment must not reach the CLI: %s", argv)
	}
	// --effort is the flag the CLI actually has. --reasoning-effort is rejected
	// as an unknown option, which surfaces as a bare exit 1 and reads exactly
	// like the agent failing at its work.
	if !strings.Contains(argv, "--effort medium") {
		t.Errorf("effort is where the cost actually was: %s", argv)
	}
	if strings.Contains(argv, "--reasoning-effort") {
		t.Errorf("--reasoning-effort is not a flag the CLI has: %s", argv)
	}

	// An explicit override wins, so an escalation does not need a code change.
	s := spec()
	s.Model, s.Effort = "anthropic/claude-opus-5", "high"
	p, err = New(s)
	if err != nil {
		t.Fatal(err)
	}
	if argv := strings.Join(p.Argv, " "); !strings.Contains(argv, "claude-opus-5") ||
		!strings.Contains(argv, "high") {
		t.Errorf("the override was ignored: %s", argv)
	}
}

// The bead id becomes a directory name and reaches a shell.
func TestAHostileBeadIdIsRefused(t *testing.T) {
	for _, bad := range []string{"", "../../etc/passwd", "wg 1", "wg;rm -rf /", "/absolute", "a/b"} {
		s := spec()
		s.Bead = bad
		if _, err := New(s); err == nil {
			t.Errorf("bead %q was accepted", bad)
		}
	}
}

func TestTheRunDirectoryIsBeneathTheCell(t *testing.T) {
	p, err := New(spec())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.RunDir, "/srv/cells/oss/") {
		t.Fatalf("run directory %s is outside the cell, so the cell's limits and ownership do not apply", p.RunDir)
	}
	if !strings.HasSuffix(p.RunDir, "wg-qw1") {
		t.Errorf("the run directory should name its bead, for whoever finds it later: %s", p.RunDir)
	}
}

func TestInstructionsAreRequired(t *testing.T) {
	s := spec()
	s.Instructions = "   "
	if _, err := New(s); err == nil {
		t.Fatal("a run with no instructions was planned")
	}
}

func TestACellRootMustBeAbsolute(t *testing.T) {
	s := spec()
	s.CellRoot = "cells/oss"
	if _, err := New(s); err == nil {
		t.Fatal("a relative cell root was accepted")
	}
}

// The plan states its model and effort rather than leaving them to be read back
// out of Argv by position, which the first dry-run got wrong.
func TestThePlanStatesItsModelAndEffort(t *testing.T) {
	p, err := New(spec())
	if err != nil {
		t.Fatal(err)
	}
	if p.Model != "anthropic/claude-sonnet-5" || p.Effort != "medium" {
		t.Fatalf("plan says model=%q effort=%q", p.Model, p.Effort)
	}
}

// One namespace (wg-8e0). Every default is <provider>/<model>, and every one
// has recorded limits — a default that cannot be planned is a role that cannot
// run, and finding that out at dispatch is finding it out too late.
func TestEveryDefaultModelIsCanonicalAndKnown(t *testing.T) {
	for role, model := range DefaultModels {
		if _, _, err := splitModel(model); err != nil {
			t.Errorf("role %q maps to %q: %v", role, model, err)
			continue
		}
		if _, ok := GatewayModels[model]; !ok {
			t.Errorf("role %q maps to %q, which has no recorded limits", role, model)
		}
	}
}

// A bare model name is the ambiguity the rename removes: "claude-sonnet-5" could
// be addressed through anthropic, bedrock or vertex, and those are different
// endpoints with different credentials.
func TestABareModelNameIsRefused(t *testing.T) {
	s := spec()
	s.Model = "claude-sonnet-5"
	_, err := New(s)
	if err == nil {
		t.Fatal("a model with no provider was accepted")
	}
	if !strings.Contains(err.Error(), "<provider>/<model>") {
		t.Errorf("the error should say what the shape is: %v", err)
	}
}

// Claude Code reaches the gateway's /anthropic path and cannot address any other
// provider. Handing it a Kimi model must fail here rather than becoming a
// warning on stderr and a run on whatever the CLI falls back to.
func TestTheClaudeRuntimeRefusesANonAnthropicModel(t *testing.T) {
	s := spec()
	s.Model = "workers-ai/@cf/moonshotai/kimi-k2.7-code"
	_, err := New(s)
	if err == nil {
		t.Fatal("a non-Anthropic model was handed to Claude Code")
	}
	if !strings.Contains(err.Error(), string(RuntimeOpenCode)) {
		t.Errorf("the error should name the runtime that can reach it: %v", err)
	}
}

// An agent with no permissions cannot act. `claude -p` is non-interactive, so
// the first end-to-end run finished "successfully" having done nothing but ask
// for approval to read its own bead — and was billed for the asking.
func TestTheAgentIsAllowedToDoItsWork(t *testing.T) {
	p, err := New(spec())
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(p.Settings), &doc); err != nil {
		t.Fatal(err)
	}
	allow := strings.Join(doc.Permissions.Allow, " ")

	// It must be able to drive the bead it was given, or the dispatch is a
	// no-op that costs money.
	if !strings.Contains(allow, "Bash(bd:") {
		t.Errorf("the agent cannot use bd, so it cannot read or close its bead: %s", allow)
	}
	if !strings.Contains(allow, "Read") {
		t.Errorf("the agent cannot read: %s", allow)
	}
}

// Short in a specific direction: read widely, write almost nothing. Unrestricted
// Bash would make every other restriction decorative, because everything else is
// reachable through it.
func TestTheAgentCannotShellOutOrPush(t *testing.T) {
	p, err := New(spec())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(p.Settings), &doc); err != nil {
		t.Fatal(err)
	}

	for _, granted := range doc.Permissions.Allow {
		if granted == "Bash" || granted == "Bash(*)" || granted == "Bash(*:*)" {
			t.Fatalf("unrestricted Bash is granted (%q), which makes every other limit decorative", granted)
		}
	}
	deny := strings.Join(doc.Permissions.Deny, " ")
	for _, must := range []string{"git push", "gh", "curl", "rm"} {
		if !strings.Contains(deny, must) {
			t.Errorf("%q is not denied; a widened allow list would silently pick it up: %s", must, deny)
		}
	}
}

func TestAnUnknownRoleHasNoToolAllowlist(t *testing.T) {
	s := spec()
	s.Role = "crew"
	if _, err := New(s); err != nil {
		t.Fatalf("crew is a known role: %v", err)
	}
}

// An explicit allowlist wins, so a signed capability can narrow it without a
// code change (plan §8.3).
func TestAnExplicitAllowlistOverridesTheDefault(t *testing.T) {
	s := spec()
	s.AllowedTools = []string{"Read"}
	p, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.AllowedTools) != 1 || p.AllowedTools[0] != "Read" {
		t.Fatalf("the capability's allowlist was ignored: %v", p.AllowedTools)
	}
	if strings.Contains(p.Settings, "Bash(bd:") {
		t.Error("the default list leaked through despite an explicit one")
	}
}

// Permissions in a settings file are IGNORED in a workspace nobody has trusted,
// and there is nobody here to accept a dialog. The plan has to name the file
// where that is recorded, or the allowlist above is decorative.
func TestThePlanNamesTheTrustFile(t *testing.T) {
	p, err := New(spec())
	if err != nil {
		t.Fatal(err)
	}
	if p.TrustFile != "/srv/cells/oss/.claude.json" {
		t.Fatalf("trust file is %q; it belongs in the cell's home, beside the credentials", p.TrustFile)
	}
	if strings.HasPrefix(p.TrustFile, p.RunDir) {
		t.Error("the trust file is inside the run directory, so it would be deleted with it")
	}
}

// max_concurrent_agents was stored from the day budget_limits existed and read
// by nothing (wg-726). The cell slice bounds PROCESSES through TasksMax, which
// is not the same thing and never was.
func TestConcurrencyCountsLiveRunsOnly(t *testing.T) {
	root := t.TempDir()
	runs := filepath.Join(root, "runs")
	if err := os.MkdirAll(filepath.Join(runs, "wg-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(runs, "wg-b"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A live run's settings file is a SIBLING of the run directories. Counting
	// it would double every run and halve the effective limit.
	if err := os.WriteFile(filepath.Join(runs, ".wg-a.settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := Concurrency(root)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("counted %d live runs, want 2 — the settings file is not a run", n)
	}
}

// A cell that has never run anything has no runs directory. That is zero, not an
// error, or the very first dispatch into a cell would be refused.
func TestConcurrencyOnAFreshCellIsZero(t *testing.T) {
	n, err := Concurrency(t.TempDir())
	if err != nil {
		t.Fatalf("a cell with no runs directory reported an error: %v", err)
	}
	if n != 0 {
		t.Fatalf("counted %d, want 0", n)
	}
}

func TestTooManyAgentsSaysTheNumbers(t *testing.T) {
	err := ErrTooManyAgents{Cell: "oss", Running: 2, Limit: 2}
	for _, want := range []string{"oss", "2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should say %q: %v", want, err)
		}
	}
}

// --- ADR-0024: the runtime is a property of the run ---

func openCodeSpec() Spec {
	s := spec()
	s.Runtime = RuntimeOpenCode
	s.Model = "workers-ai/@cf/moonshotai/kimi-k2.7-code"
	s.GatewayBaseURL = "https://gateway.ai.cloudflare.com/v1/acct/workgraph-staging-oss"
	return s
}

// The default must not move by accident. ADR-0024 makes another runtime
// possible; it migrates nothing, because changing what executes model output
// against our repositories deserves a decision rather than a side effect.
func TestTheDefaultRuntimeIsStillClaude(t *testing.T) {
	p, err := New(spec())
	if err != nil {
		t.Fatal(err)
	}
	if p.Runtime != RuntimeClaude {
		t.Errorf("runtime = %q, want %q", p.Runtime, RuntimeClaude)
	}
	if p.Argv[0] != "claude" {
		t.Errorf("argv[0] = %q, want claude", p.Argv[0])
	}
}

// The credential must not be readable by the agent it authorises. An agent with
// Read in its own working directory found the gateway token once already and
// said so; it behaved well and the next one might not. This has to hold for
// EVERY runtime, which is the point of asserting it in a loop rather than once.
func TestNoRuntimePutsTheCredentialWhereTheAgentCanReadIt(t *testing.T) {
	for _, s := range []Spec{spec(), openCodeSpec()} {
		p, err := New(s)
		if err != nil {
			t.Fatalf("%s: %v", s.Runtime, err)
		}
		if !strings.Contains(p.Settings, s.GatewayToken) {
			t.Fatalf("%s: the config should carry the token; this test proves nothing otherwise", p.Runtime)
		}
		if strings.HasPrefix(p.SettingsPath, p.RunDir+string(filepath.Separator)) {
			t.Errorf("%s: config %q is inside the run directory %q, where the agent can read it",
				p.Runtime, p.SettingsPath, p.RunDir)
		}
	}
}

// OpenCode is told the model's limits or it asks for 32000 output tokens and a
// smaller model refuses the whole request before doing any work.
func TestOpenCodeCarriesTheModelLimits(t *testing.T) {
	p, err := New(openCodeSpec())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Provider map[string]struct {
			Options struct {
				BaseURL string            `json:"baseURL"`
				Headers map[string]string `json:"headers"`
			} `json:"options"`
			Models map[string]struct {
				Limit map[string]int `json:"limit"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(p.Settings), &doc); err != nil {
		t.Fatalf("the config is not valid JSON: %v", err)
	}
	prov := doc.Provider[openCodeProvider]
	if got := prov.Models["workers-ai/@cf/moonshotai/kimi-k2.7-code"].Limit["output"]; got != 8192 {
		t.Errorf("output limit = %d, want 8192", got)
	}
	// /compat, not /anthropic: that endpoint is what accepts provider/model
	// naming, and it is where dynamic routes will live.
	if !strings.HasSuffix(prov.Options.BaseURL, "/compat") {
		t.Errorf("baseURL = %q, want a /compat suffix", prov.Options.BaseURL)
	}
	// Both headers or neither. The credential and the attribution are one
	// variable: a run that authenticates but arrives untagged is worse than one
	// that fails, because it looks like success from outside every budget.
	if prov.Options.Headers["cf-aig-authorization"] == "" {
		t.Error("the gateway credential is missing")
	}
	if !strings.Contains(prov.Options.Headers["cf-aig-metadata"], `"bead"`) {
		t.Errorf("attribution is missing from the headers: %q", prov.Options.Headers["cf-aig-metadata"])
	}
}

// OpenCode allows everything by default, which is the opposite of Claude Code.
// The config has to deny first and allow back, and bash has to be bd and
// nothing else — no git push, no curl, no shelling out to reach them.
func TestOpenCodeDeniesFirstAndAllowsBackOnlyBd(t *testing.T) {
	p, err := New(openCodeSpec())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Permission struct {
			Star string            `json:"*"`
			Bash map[string]string `json:"bash"`
			Read string            `json:"read"`
		} `json:"permission"`
	}
	if err := json.Unmarshal([]byte(p.Settings), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Permission.Star != "deny" {
		t.Errorf(`permission["*"] = %q, want deny — without a catch-all deny, `+
			`anything OpenCode adds later is allowed by default`, doc.Permission.Star)
	}
	if doc.Permission.Bash["*"] != "deny" {
		t.Errorf(`permission.bash["*"] = %q, want deny`, doc.Permission.Bash["*"])
	}
	if doc.Permission.Bash["bd *"] != "allow" {
		t.Error("the agent cannot drive its own bead without bd")
	}
	if doc.Permission.Read != "allow" {
		t.Error("an agent that cannot read cannot work")
	}
}

// A model nobody has recorded limits for is refused rather than run with
// OpenCode's default, which a smaller model rejects outright.
func TestAnUnknownModelIsRefusedForOpenCode(t *testing.T) {
	s := openCodeSpec()
	s.Model = "workers-ai/@cf/nobody/never-heard-of-it"
	if _, err := New(s); err == nil {
		t.Error("a model with no known limits was planned anyway")
	}
}

// Claude Code's spellings are not the gateway's. Handing "sonnet" to /compat
// would fail a long way from here, so it fails at planning instead.
func TestOpenCodeRefusesToGuessAModel(t *testing.T) {
	s := openCodeSpec()
	s.Model = ""
	_, err := New(s)
	if err == nil {
		t.Fatal("the opencode runtime defaulted to a Claude Code model name")
	}
	if !strings.Contains(err.Error(), "explicit model") {
		t.Errorf("the error should say a model is required: %v", err)
	}
}

func TestAnUnknownRuntimeIsRefused(t *testing.T) {
	s := spec()
	s.Runtime = "telepathy"
	if _, err := New(s); err == nil {
		t.Error("an unknown runtime was accepted")
	}
}

// OpenCode keeps sessions in a SQLite database under XDG_DATA_HOME. Shared
// between runs it wedges, and every run after the first dies with "Session not
// found" before reaching the model — which reads as the model refusing to use
// tools. Four candidates were recorded as tool-use failures that way.
func TestOpenCodeGetsItsOwnStateDirectory(t *testing.T) {
	p, err := New(openCodeSpec())
	if err != nil {
		t.Fatal(err)
	}
	if p.StatePath == "" {
		t.Fatal("no state directory; runs will share one and wedge after the first")
	}
	if p.Env["XDG_DATA_HOME"] != p.StatePath {
		t.Errorf("XDG_DATA_HOME = %q, want the state directory %q", p.Env["XDG_DATA_HOME"], p.StatePath)
	}
	// Beside the run, not inside it: the agent has no business reading the
	// runtime's own scratch, on the same reasoning as the config file.
	if strings.HasPrefix(p.StatePath, p.RunDir+string(filepath.Separator)) {
		t.Errorf("state %q is inside the run directory %q", p.StatePath, p.RunDir)
	}
	// Per bead, or two concurrent runs in a cell share it and reintroduce the
	// bug from the other direction.
	if !strings.Contains(p.StatePath, "wg-qw1") {
		t.Errorf("state path %q is not per-run", p.StatePath)
	}
}

// The claude runtime keeps no state we isolate, so it must not ask for a
// directory that teardown would then remove for nothing.
func TestTheClaudeRuntimeHasNoStateDirectory(t *testing.T) {
	p, err := New(spec())
	if err != nil {
		t.Fatal(err)
	}
	if p.StatePath != "" {
		t.Errorf("claude asked for a state directory: %q", p.StatePath)
	}
}

// The graph follows the BEAD, not the rig the work runs in.
//
// Routing by repository split the two apart: sa-4yn was filed in the sandbox
// rig and is about PortalJS, so it now runs in the portaljs rig -- where
// `bd show sa-4yn` answers `no issue found matching "sa-4yn"`, because a bead
// graph is per-rig and one cannot see another's. The working tree has to be the
// rig that holds the code and the graph has to be the rig that holds the bead.
func TestTheGraphFollowsTheBeadAcrossRigs(t *testing.T) {
	root := t.TempDir()
	town := func(rig, prefix, kind string) {
		if err := os.MkdirAll(filepath.Join(root, "town", rig), 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"type":"` + kind + `","name":"` + rig + `","beads":{"prefix":"` + prefix + `"}}`
		if err := os.WriteFile(filepath.Join(root, "town", rig, "config.json"),
			[]byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	town("sandbox", "sa", "rig")
	town("portaljs", "por7", "rig")
	// Not a rig, and it carries a config.json: it must never be chosen however
	// its contents read.
	town("settings", "sa", "town-settings")

	for _, c := range []struct{ name, bead, rig, want string }{
		{"a bead from another rig brings its own graph", "sa-4yn", "portaljs", "sandbox"},
		{"a bead from the run's own rig keeps it", "por7-abc", "portaljs", "portaljs"},
		{"an unknown prefix falls back to the run's rig", "zz9-abc", "portaljs", "portaljs"},
		{"an id with no prefix falls back too", "4yn", "portaljs", "portaljs"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := spec()
			s.CellRoot, s.Bead, s.Rig = root, c.bead, c.rig
			p, err := New(s)
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(root, "town", c.want)
			if p.BeadsDir != want {
				t.Errorf("beads dir is %q, want %q", p.BeadsDir, want)
			}
			if p.Env["BEADS_DIR"] != filepath.Join(want, ".beads") {
				t.Errorf("BEADS_DIR is %q, so bd would look in the wrong place", p.Env["BEADS_DIR"])
			}
		})
	}
}

// The agent must reach BOTH rigs: the code and the graph.
//
// These are the same directory in the ordinary case, and were assumed to be
// until routing by repository separated them. Giving only one is a run that
// costs full price and cannot work. With only the bead's rig, sa-4yn ran
// against PortalJS for 32.4 cents and reported "no PortalJS source code
// accessible anywhere in this environment", having searched town/sandbox --
// the one directory it could reach -- while town/portaljs sat outside its
// sandbox.
func TestTheAgentCanReachBothTheCodeAndTheGraph(t *testing.T) {
	root := t.TempDir()
	for _, r := range []struct{ rig, prefix string }{{"sandbox", "sa"}, {"portaljs", "por7"}} {
		if err := os.MkdirAll(filepath.Join(root, "town", r.rig), 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"type":"rig","name":"` + r.rig + `","beads":{"prefix":"` + r.prefix + `"}}`
		if err := os.WriteFile(filepath.Join(root, "town", r.rig, "config.json"),
			[]byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := spec()
	s.CellRoot, s.Bead, s.Rig = root, "sa-4yn", "portaljs"
	p, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		filepath.Join(root, "town", "portaljs"), // the code
		filepath.Join(root, "town", "sandbox"),  // the graph
	} {
		if !strings.Contains(p.Settings, want) {
			t.Errorf("%s is not in additionalDirectories, so the sandbox confines the agent away from it", want)
		}
	}

	// And when they are the same rig, the path appears once. A settings file
	// naming one directory twice invites the reader to wonder which is real.
	s2 := spec()
	s2.CellRoot, s2.Bead, s2.Rig = root, "por7-abc", "portaljs"
	p2, err := New(s2)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(p2.Settings, filepath.Join(root, "town", "portaljs")); n != 1 {
		t.Errorf("the rig appears %d times in the settings, want once", n)
	}
}

// The two runtimes grant the same commands, because the bash block is derived
// from the same allowlist rather than written out twice.
//
// It WAS written out by hand as {"*": "deny", "bd *": "allow"}, which meant
// adding a command to DefaultTools granted it under the claude runtime and
// silently not under opencode. OpenCode is the default runtime now, so that
// drift would have been the live behaviour: an agent told it had a browser,
// with no permission to run it.
func TestBothRuntimesGrantTheSameCommands(t *testing.T) {
	got := bashPermissions(DefaultTools["polecat"])

	// Deny first. OpenCode matches with the LAST pattern winning, so a
	// catch-all written after the allows would deny everything while reading
	// exactly the same.
	if got["*"] != "deny" {
		t.Errorf("the catch-all is %q, want deny", got["*"])
	}
	for _, want := range []string{"bd *", "wg-browse *"} {
		if got[want] != "allow" {
			t.Errorf("%q is %q, want allow", want, got[want])
		}
	}
	// And nothing else is allowed. A permission block that grants more than
	// the allowlist is the drift this function exists to prevent.
	for pattern, decision := range got {
		if decision != "allow" {
			continue
		}
		if pattern != "bd *" && pattern != "wg-browse *" {
			t.Errorf("%q is allowed and is not in the allowlist", pattern)
		}
	}

	// A role with no Bash entries gets deny and nothing else, rather than an
	// empty block — which OpenCode would read as no restriction at all.
	bare := bashPermissions([]string{"Read", "Grep"})
	if len(bare) != 1 || bare["*"] != "deny" {
		t.Errorf("a role with no shell got %v, want only a deny", bare)
	}
}

// The browser reaches the agent as `wg-browse`, never as the browser itself.
// The command is what decides which addresses may be fetched; the binary would
// fetch anything, including this node's cloud metadata, which answers 200.
func TestTheAgentGetsTheBrowseCommandAndNotTheBrowser(t *testing.T) {
	for _, role := range []string{"polecat", "crew"} {
		tools := DefaultTools[role]
		if !slices.Contains(tools, "Bash(wg-browse:*)") {
			t.Errorf("%s cannot browse at all: %v", role, tools)
		}
		for _, forbidden := range []string{
			"Bash(chrome-headless-shell:*)", "Bash(chrome:*)", "Bash(chromium:*)",
			"Bash(curl:*)", "Bash(*)",
		} {
			if slices.Contains(tools, forbidden) {
				t.Errorf("%s is granted %s, which bypasses the address policy", role, forbidden)
			}
		}
	}
}
