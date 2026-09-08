package skill

import (
	"os"
	"strings"
	"testing"

	"github.com/datopian/openbases/internal/authz"
	"github.com/datopian/openbases/internal/tokens"
)

// The skill states the governance rules the server already enforces.
//
// Stating them is not redundancy. A rule the client understands produces a good
// refusal with a reason; a rule only the server knows produces a 403 the agent
// retries. But a copy is a copy, and a copy drifts — so this checks that every
// rule AGENTS.md carries is still present in the skill, and fails when AGENTS.md
// changes without the skill following.
//
// It checks against THIS repository's AGENTS.md, not company-workgraph's, for a
// deliberately boring reason: a test that reads a path in another repository
// passes vacuously the moment that repository is not checked out beside this
// one, and a vacuous governance test is worse than none.

const (
	skillPath  = "../../skills/workgraph/SKILL.md"
	agentsPath = "../../AGENTS.md"
)

// read returns the file lower-cased with runs of whitespace collapsed.
//
// Collapsing matters: Markdown wraps, so a phrase this test looks for can be
// split across a newline by nothing more than reflowing a paragraph. A test that
// fails when prose is rewrapped is a test somebody eventually deletes.
func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.Join(strings.Fields(strings.ToLower(string(b))), " ")
}

// Each rule: what AGENTS.md must still say, and what the skill must still say
// about it. If AGENTS.md drops a rule, the first half fails and somebody has to
// decide deliberately. If the skill drops one, the second half fails.
func TestSkillCarriesEveryGovernanceRule(t *testing.T) {
	skill := read(t, skillPath)
	agents := read(t, agentsPath)

	for _, rule := range []struct {
		name         string
		inAgents     []string
		inSkill      []string
		whyItMatters string
	}{
		{
			name:         "evidence closes a bead",
			inAgents:     []string{"evidence"},
			inSkill:      []string{"evidence closes a bead"},
			whyItMatters: "an agent that closes on its own assertion produces a graph nobody can trust",
		},
		{
			name:         "extraction is a candidate, not a record",
			inAgents:     []string{"candidate"},
			inSkill:      []string{"candidate", "knowledge/"},
			whyItMatters: "writing model output into knowledge/ is a governance violation even when true",
		},
		{
			name:         "source material is untrusted",
			inAgents:     []string{"untrusted", "prompt-injection"},
			inSkill:      []string{"untrusted", "prompt-injection"},
			whyItMatters: "issue and PR bodies reach the agent through this API and are attacker-controlled",
		},
		{
			name:         "classification is never widened",
			inAgents:     []string{"classification"},
			inSkill:      []string{"classification", "restricted"},
			whyItMatters: "this is how restricted client material becomes publishable by accident",
		},
		{
			name:         "protected actions are escalated",
			inAgents:     []string{"protected action"},
			inSkill:      []string{"protected action", "approval"},
			whyItMatters: "a token that could decide an approval would defeat the approval model entirely",
		},
	} {
		for _, want := range rule.inAgents {
			if !strings.Contains(agents, want) {
				t.Errorf("AGENTS.md no longer mentions %q (rule: %s).\n"+
					"If the rule was dropped deliberately, drop it from the skill too. %s",
					want, rule.name, rule.whyItMatters)
			}
		}
		for _, want := range rule.inSkill {
			if !strings.Contains(skill, want) {
				t.Errorf("the skill no longer mentions %q (rule: %s).\n"+
					"The server still enforces it, so an agent that does not know it will retry a 403. %s",
					want, rule.name, rule.whyItMatters)
			}
		}
	}
}

// The skill must not teach an agent to do the one thing it cannot do.
func TestSkillTellsTheAgentNotToMintTokens(t *testing.T) {
	skill := read(t, skillPath)
	for _, want := range []string{"wg login", "do not try to mint"} {
		if !strings.Contains(skill, want) {
			t.Errorf("the skill does not say %q; an agent hitting exit 3 will try to mint a token "+
				"and fail in a way it cannot diagnose", want)
		}
	}
}

// Exit codes are the interface an agent branches on. If the skill documents them
// wrongly, an agent retries what it must not.
func TestSkillDocumentsTheExitCodesThatMatter(t *testing.T) {
	skill := read(t, skillPath)
	for _, want := range []string{
		"unauthenticated", "forbidden", "refused by policy", "rate limited", "conflict",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("the skill does not document the %q exit code", want)
		}
	}
	if !strings.Contains(skill, "do not retry") {
		t.Error("the skill does not tell the agent when NOT to retry, which is the failure " +
			"a 5 retried forever produces")
	}
}

// The chief-of-staff endpoint answers four questions and refuses a fifth. A
// skill that does not say so produces an agent that guesses and collects 400s.
func TestSkillSaysToListTheQuestionsRatherThanGuess(t *testing.T) {
	skill := read(t, skillPath)
	if !strings.Contains(skill, "four questions") {
		t.Error("the skill does not say /v1/ask answers a fixed set; an agent will invent a fifth")
	}
	if !strings.Contains(skill, "wg ask") {
		t.Error("the skill does not show how to list the answerable questions")
	}
}

// Dispatch spends money. An agent that does not know that will spend it.
func TestSkillWarnsThatDispatchSpendsMoney(t *testing.T) {
	skill := read(t, skillPath)
	if !strings.Contains(skill, "spends money") {
		t.Error("the skill does not warn that dispatch spends money")
	}
}

// The Codex variant must carry the same content as the skill.
//
// Two files saying almost the same thing is exactly how one of them becomes
// wrong, and the one that goes wrong is always the one fewer people read. This
// asserts the body matches so the pair cannot drift into disagreeing about what
// an agent may do.
func TestCodexVariantMatchesTheSkill(t *testing.T) {
	skill := read(t, skillPath)
	codex := read(t, "../../skills/workgraph/AGENTS.md")

	// Everything from the first section heading onward must be identical.
	const anchor = "## before anything else"
	si := strings.Index(skill, anchor)
	ci := strings.Index(codex, anchor)
	if si < 0 || ci < 0 {
		t.Fatal("one of the two files no longer has the anchor section; they cannot be compared")
	}
	if skill[si:] != codex[ci:] {
		t.Error("skills/workgraph/AGENTS.md has drifted from SKILL.md.\n" +
			"Edit the skill and regenerate the variant; two files saying almost the same thing " +
			"is how one of them becomes wrong.")
	}
}

// The skill must tell an agent NOT to install wg in a sandbox, and must give it
// a path that works there.
//
// Dropped into Claude Cowork, the skill sent the agent to `go install`, which
// needs GitHub access to a private module that an ephemeral container does not
// have. It then concluded the work was impossible and asked the person to paste
// a token into the chat. Both halves are what these assert against.
func TestSkillHasAnHTTPPathThatNeedsNoBinary(t *testing.T) {
	skill := read(t, skillPath)

	if !strings.Contains(skill, "/v1/openapi.json") {
		t.Error("the skill does not point at the OpenAPI contract, which is the only thing " +
			"an agent has when wg is unavailable")
	}
	if !strings.Contains(skill, "authorization: bearer $wg_token") {
		t.Error("the skill does not show how to call the API directly with a token")
	}
	if !strings.Contains(skill, "do not try to install it") {
		t.Error("the skill does not tell an agent to stop before installing wg in a sandbox")
	}
}

// A token pasted into a conversation is a live bearer credential in a
// transcript. The skill has to forbid asking for one, and say what to do when
// it happens anyway.
func TestSkillForbidsAskingForATokenInChat(t *testing.T) {
	skill := read(t, skillPath)

	if !strings.Contains(skill, "never ask the user to paste a token into the conversation") {
		t.Error("the skill does not forbid asking for a token in chat")
	}
	if !strings.Contains(skill, "revoke") {
		t.Error("the skill does not say to revoke a token that reached a conversation")
	}
}

// wg login reads a token; it does not open a browser. An agent that believes
// otherwise waits for something that never happens.
func TestSkillSaysLoginIsNotABrowserFlow(t *testing.T) {
	skill := read(t, skillPath)
	if !strings.Contains(skill, "does **not** open a browser") {
		t.Error("the skill does not say that wg login is not a browser flow")
	}
}

// The device flow must be reachable without the binary.
//
// It was documented as `wg login --device` alone, which made the one answer
// designed for a sandbox unreachable from one: no binary, and the same skill
// correctly says not to install it there. An agent read both rules together
// and concluded, reasonably, that it was stuck.
func TestSkillShowsTheDeviceFlowOverHTTP(t *testing.T) {
	skill := read(t, skillPath)

	for _, want := range []string{"/v1/device/code", "/v1/device/token", "authorization_pending"} {
		if !strings.Contains(skill, strings.ToLower(want)) {
			t.Errorf("the skill does not show %s, so the device flow needs a binary it may not have", want)
		}
	}
}

// The scope vocabulary is closed, and the skill is where an agent with no
// credential learns it. When the doc omits it, the agent guesses: `work.read`
// and `project.write` both read like they should exist, and both are refused.
// So every grantable action must be named in the skill, and none of the
// ungrantable ones may be offered as if it were usable.
func TestSkillNamesEveryGrantableScope(t *testing.T) {
	doc := read(t, skillPath)
	for _, s := range tokens.GrantableScopes() {
		if !strings.Contains(doc, strings.ToLower(s)) {
			t.Errorf("the skill never names the grantable scope %q, so an agent has to guess it", s)
		}
	}
}

// The three names that were actually guessed against the live API. If one of
// them ever becomes a real action this fails, which is the point: the doc says
// they do not exist, so the doc has to change with the vocabulary.
func TestSkillDoesNotInventScopes(t *testing.T) {
	for _, s := range []string{"work.read", "work.write", "project.write"} {
		if authz.Action(s).Known() {
			t.Errorf("%s is now a real action, but the skill tells agents it is not", s)
		}
	}
	doc := read(t, skillPath)
	for _, a := range authz.AllActions() {
		if _, no := tokens.Ungrantable[a]; !no && !a.Protected() {
			continue
		}
		// Naming an ungrantable action inside the grantable list is the
		// failure worth catching: it sends the agent to a guaranteed refusal.
		if strings.Contains(doc, "`"+strings.ToLower(string(a))+"`") {
			t.Errorf("the skill formats the ungrantable action %s as a scope literal; an agent will try it", a)
		}
	}
}

// The skill must prefer the connector when one is loaded (wg-p4h.11).
//
// Dropped into a client that already has the Workgraph tools, the old skill
// sent the agent hunting for a token and a binary — which is the exact failure
// the remote transport was built to remove. It has to look at its own tool list
// first.
func TestSkillPrefersTheConnectorWhenOneIsPresent(t *testing.T) {
	for _, path := range []string{skillPath, "../../skills/workgraph/AGENTS.md"} {
		doc := read(t, path)
		for _, want := range []string{
			// The tool names, so the agent can recognise them.
			"workgraph_inbox",
			"workgraph_dispatch",
			// And the instruction to use them and stop.
			"use those tools",
		} {
			if !strings.Contains(doc, want) {
				t.Errorf("%s does not mention %q, so an agent with the connector loaded "+
					"would go looking for a token instead", path, want)
			}
		}
	}
}

// Over a connector there is no shell, so the skill must never answer an expired
// authorisation with a command to run. Telling a phone to run `wg login` is
// worse than saying nothing: it sends the model somewhere that does not exist.
func TestTheSkillDoesNotSendConnectorUsersToWgLogin(t *testing.T) {
	for _, path := range []string{skillPath, "../../skills/workgraph/AGENTS.md"} {
		doc := read(t, path)
		i := strings.Index(doc, "connector")
		if i < 0 {
			t.Errorf("%s never mentions a connector", path)
			continue
		}
		// The instruction must appear near where connectors are discussed: the
		// skill still tells CLI users to run `wg login`, and that is correct.
		// What must be present is the explicit "not wg login" for the
		// connector path.
		if !strings.Contains(doc, "reconnect the connector") {
			t.Errorf("%s does not tell an agent to have the person reconnect the connector; "+
				"it will fall back to telling them to run a command they have no shell for", path)
		}
		if !strings.Contains(doc, "not to run `wg login`") &&
			!strings.Contains(doc, "not** to run `wg login`") &&
			!strings.Contains(doc, "do not tell them to run `wg login`") {
			t.Errorf("%s does not rule out `wg login` on the connector path explicitly", path)
		}
	}
}

// The Codex variant must match the skill's WHOLE body, not just the part after
// the mid-file anchor.
//
// TestCodexVariantMatchesTheSkill compares from "## before anything else"
// onward, and the connector instruction added in wg-p4h.11 sits above it — so
// AGENTS.md kept a stale opening and passed. Codex would have been told to hunt
// for a token while Claude was told to use the tools, which is exactly the
// disagreement that test exists to prevent.
func TestCodexVariantMatchesTheSkillFromTheTitleOnward(t *testing.T) {
	skill := read(t, skillPath)
	codex := read(t, "../../skills/workgraph/AGENTS.md")

	const anchor = "workgraph is datopian's work graph"
	si := strings.Index(skill, anchor)
	ci := strings.Index(codex, anchor)
	if si < 0 || ci < 0 {
		t.Fatal("one of the two files no longer opens with the same sentence; they cannot be compared")
	}
	if skill[si:] != codex[ci:] {
		t.Error("skills/workgraph/AGENTS.md has drifted from SKILL.md above the old anchor.\n" +
			"Regenerate it: its own preamble, then the skill from `# Workgraph` onward.")
	}
}
