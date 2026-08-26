package runner

import (
	"encoding/json"
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
	if !strings.Contains(argv, "--model sonnet") {
		t.Errorf("polecat should run on its tier: %s", argv)
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
	s.Model, s.Effort = "claude-opus-5", "high"
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
	if p.Model != "sonnet" || p.Effort != "medium" {
		t.Fatalf("plan says model=%q effort=%q", p.Model, p.Effort)
	}
}

// The tier names must be ones the CLI accepts. Gas Town spells them
// "claude-sonnet"; the CLI answers that with "not a model this version of Claude
// Code recognizes" and then runs anyway on a default, so the wrong tier is a
// line on stderr rather than a failure.
func TestModelNamesAreOnesTheCLIAccepts(t *testing.T) {
	for role, model := range DefaultModels {
		if strings.HasPrefix(model, "claude-") {
			t.Errorf("role %q maps to %q, which is Gas Town's spelling, not the CLI's", role, model)
		}
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
