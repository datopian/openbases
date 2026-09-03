package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The tool list IS the prompt — it is what a model reads before deciding — so
// these assertions are about what the model will be told, not about plumbing.
//
// Moved here from cmd/wg when the tools moved (wg-p4h.11). Every property the
// CLI's tests asserted still holds, and now holds for BOTH transports: the
// remote server at /mcp is built from this same data, so a tool that starts
// lying about spending money lies to a phone as well as to a laptop.

// Curated, not generated. Two dozen tools would give a model no judgement, and
// this test is what stops "expose everything" happening by increment.
func TestToolSetStaysSmall(t *testing.T) {
	if len(Tools()) > 8 {
		t.Fatalf("%d tools. The list is the prompt: past a handful, a model chooses worse "+
			"rather than doing more. Add to the CLI instead.", len(Tools()))
	}
}

// A tool that always fails teaches the model to keep trying it.
func TestNoToolExistsForSomethingNoCredentialCanEverDo(t *testing.T) {
	for _, tl := range Tools() {
		n := strings.ToLower(tl.Name)
		switch {
		case strings.Contains(n, "approv"):
			t.Errorf("%s: no credential may decide an approval, so this tool would always fail", tl.Name)
		case strings.Contains(n, "token") && !strings.Contains(n, "workgraph_work"):
			t.Errorf("%s: minting needs an interactive session; a tool teaches the model to attempt "+
				"something it cannot do", tl.Name)
		case strings.Contains(n, "merge") || strings.Contains(n, "deploy") || strings.Contains(n, "secret"):
			t.Errorf("%s: protected actions are escalated to a human, not offered as a tool", tl.Name)
		}
	}
}

// Spending money must be visible in the description, because that is the only
// place the model sees it before calling.
func TestSpendingToolsSayTheySpend(t *testing.T) {
	for _, name := range []string{"workgraph_file_work", "workgraph_dispatch"} {
		tl := byName(t, name)
		if !strings.Contains(strings.ToUpper(tl.Description), "SPENDS MONEY") {
			t.Errorf("%s does not say it spends money; the model has no other way to know", name)
		}
		if !strings.Contains(strings.ToLower(tl.Description), "confirm") {
			t.Errorf("%s does not tell the model to confirm first", name)
		}
	}
}

// The annotations are what make a CLIENT prompt, which is the other half of the
// same guarantee: the description persuades the model, the annotation persuades
// the application. A tool that spends money and claims to be read-only would be
// run without asking anybody.
func TestSpendingToolsAreNotAnnotatedReadOnly(t *testing.T) {
	for _, r := range Routes {
		tl := byName(t, r.Tool)
		if tl.Annotations == nil {
			t.Errorf("%s has no annotations, so a client has nothing to prompt on", r.Tool)
			continue
		}
		if r.SpendsMoney {
			if tl.Annotations.ReadOnlyHint {
				t.Errorf("%s spends money and is annotated read-only; clients will not prompt", r.Tool)
			}
			if tl.Annotations.DestructiveHint == nil || !*tl.Annotations.DestructiveHint {
				t.Errorf("%s spends money and is not annotated destructive", r.Tool)
			}
		}
		if r.ReadOnly && !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s only reads but is not annotated read-only, so clients will prompt "+
				"for something harmless and teach people to click through prompts", r.Tool)
		}
	}
}

// The ask tool must tell the model the question set is fixed, or it will invent
// a fifth question and collect 400s.
func TestAskToolSaysTheQuestionSetIsFixed(t *testing.T) {
	d := strings.ToLower(byName(t, "workgraph_ask").Description)
	if !strings.Contains(d, "fixed") || !strings.Contains(d, "list them") {
		t.Error("workgraph_ask does not tell the model to list the questions rather than invent one")
	}
}

func TestEveryToolHasANameDescriptionAndNamespace(t *testing.T) {
	seen := map[string]bool{}
	for _, tl := range Tools() {
		if tl.Name == "" || tl.Description == "" {
			t.Errorf("%+v is missing a name or description", tl)
		}
		if seen[tl.Name] {
			t.Errorf("duplicate tool name %q", tl.Name)
		}
		seen[tl.Name] = true
		if !strings.HasPrefix(tl.Name, "workgraph_") {
			t.Errorf("%s is not namespaced; a model with several servers loaded needs to tell them apart",
				tl.Name)
		}
	}
}

// The route table and the advertised tools must describe the same set. They are
// two lists that have to agree, which is a bug waiting unless something checks.
func TestRoutesAndToolsAgree(t *testing.T) {
	if len(Routes) != len(Tools()) {
		t.Fatalf("%d routes for %d tools", len(Routes), len(Tools()))
	}
	for i, r := range Routes {
		if Tools()[i].Name != r.Tool {
			t.Errorf("position %d: route %q, tool %q — the order is what the model reads",
				i, r.Tool, Tools()[i].Name)
		}
		switch r.Method {
		case http.MethodGet:
			if !r.ReadOnly {
				t.Errorf("%s is a GET and is not marked read-only", r.Tool)
			}
		case http.MethodPost:
			if r.ReadOnly {
				t.Errorf("%s is a POST and is marked read-only", r.Tool)
			}
		default:
			t.Errorf("%s uses %s; the tool set is reads and creates only", r.Tool, r.Method)
		}
		if !strings.HasPrefix(r.Path, "/v1/") {
			t.Errorf("%s maps to %q, which is not a /v1 route", r.Tool, r.Path)
		}
	}
}

// The model should be told where it stands on retrying, rather than guessing.
// Guessing means either a retry storm or giving up on something that would have
// worked.
func TestRefusalTextNamesTheRetryPosition(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, want string
		hasShell   bool
	}{
		{"scope", 403, `{"error":"nope","code":"token_scope_insufficient"}`, "do not retry", true},
		{"policy", 403, `{"error":"over budget"}`, "do not retry", true},
		{"rate", 429, `{"error":"slow down","code":"rate_limited"}`, "wait before retrying", true},
		{"expired with a shell", 401, `{"error":"unauthorized"}`, "wg login", true},
		// The connector path has no login command. Telling the model to run
		// one sends it hunting for a shell that does not exist, which is the
		// exact failure that made the remote transport necessary.
		{"expired without a shell", 401, `{"error":"unauthorized"}`, "reconnect it", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := refusalText(tc.status, []byte(tc.body), tc.hasShell)
			if !strings.Contains(got, tc.want) {
				t.Errorf("status %d produced %q, which does not tell the model %q",
					tc.status, got, tc.want)
			}
		})
	}
}

// Without a shell, the model must never be told to run a command.
func TestTheConnectorPathNeverTellsTheModelToRunWgLogin(t *testing.T) {
	got := refusalText(401, []byte(`{"error":"unauthorized"}`), false)
	if strings.Contains(got, "wg login") {
		t.Errorf("a connector client has no shell, and this told it to run a command: %q", got)
	}
}

// 403 is two problems wearing one status and they have different fixes: a scope
// refusal needs a different credential, a policy refusal needs a person.
func TestClassifyTellsScopeAndPolicyApart(t *testing.T) {
	if got := Classify(403, []byte(`{"code":"role_grant_missing"}`)); got != RefusalForbidden {
		t.Errorf("a role refusal classified as %d, want %d", got, RefusalForbidden)
	}
	if got := Classify(403, []byte(`{"error":"the budget for this bead is spent"}`)); got != RefusalPolicy {
		t.Errorf("a budget refusal classified as %d, want %d", got, RefusalPolicy)
	}
	if got := Classify(204, nil); got != RefusalNone {
		t.Errorf("a 204 classified as %d, want none", got)
	}
	if got := Classify(500, nil); got != RefusalOther {
		t.Errorf("a 500 classified as %d, want other", got)
	}
}

// stubCaller records what a tool asked for and answers with what it is given.
type stubCaller struct {
	method, path string
	body         any
	status       int
	resp         string
	err          error
}

func (s *stubCaller) Call(_ context.Context, method, path string, body any) (int, []byte, error) {
	s.method, s.path, s.body = method, path, body
	return s.status, []byte(s.resp), s.err
}

// The tools must reach the routes the table says they do. This is the mapping
// the whole package exists to share, so it is asserted end to end through the
// SDK rather than by reading the table back.
func TestToolsCallTheRoutesTheTableNames(t *testing.T) {
	for _, tc := range []struct {
		tool, method, path string
		args               map[string]any
	}{
		{"workgraph_inbox", "GET", "/v1/inbox", nil},
		{"workgraph_ask", "GET", "/v1/ask", nil},
		{"workgraph_ask", "GET", "/v1/ask?q=what+changed", map[string]any{"question": "what changed"}},
		{"workgraph_work_list", "GET", "/v1/work", nil},
		{"workgraph_project_list", "GET", "/v1/projects", nil},
		{"workgraph_file_work", "POST", "/v1/work/plan", map[string]any{"brief": "do a thing"}},
		{"workgraph_dispatch", "POST", "/v1/work/wg-abc/dispatch", map[string]any{"bead": "wg-abc"}},
	} {
		t.Run(tc.tool+" "+tc.path, func(t *testing.T) {
			stub := &stubCaller{status: 200, resp: `{"ok":true}`}
			res := call(t, stub, tc.tool, tc.args)
			if res.IsError {
				t.Fatalf("the call failed: %s", text(res))
			}
			if stub.method != tc.method || stub.path != tc.path {
				t.Errorf("called %s %s, want %s %s", stub.method, stub.path, tc.method, tc.path)
			}
		})
	}
}

// A bead id reaches a path segment, so it is escaped. The CLI interpolated it
// raw, which was safe only because the value had just come from the server.
func TestADispatchBeadIsEscapedIntoThePath(t *testing.T) {
	stub := &stubCaller{status: 200, resp: `{}`}
	call(t, stub, "workgraph_dispatch", map[string]any{"bead": "wg-a/../../v1/tokens"})
	if strings.Contains(stub.path, "../") {
		t.Errorf("an unescaped bead id reached the path: %q", stub.path)
	}
}

// Refusals come back as readable content rather than a transport error.
func TestARefusalIsAReadableResultNotAnError(t *testing.T) {
	stub := &stubCaller{status: 403, resp: `{"error":"this credential may not","code":"role_grant_missing"}`}
	res := call(t, stub, "workgraph_dispatch", map[string]any{"bead": "wg-abc"})
	if !res.IsError {
		t.Error("a refusal should be flagged isError so the model knows it failed")
	}
	if !strings.Contains(text(res), "this credential may not") {
		t.Errorf("the server's own words did not survive: %q", text(res))
	}
	if !strings.Contains(text(res), "do not retry") {
		t.Errorf("the refusal does not name the retry position: %q", text(res))
	}
}

// Missing required arguments are refused before anything is called, so a model
// that forgets the brief does not file an empty planning job.
func TestAnEmptyBriefIsRefusedBeforeCalling(t *testing.T) {
	stub := &stubCaller{status: 200, resp: `{}`}
	res := call(t, stub, "workgraph_file_work", map[string]any{"brief": "   "})
	if !res.IsError {
		t.Fatal("an empty brief was accepted")
	}
	if stub.method != "" {
		t.Errorf("it called %s %s anyway", stub.method, stub.path)
	}
}

// The observer sees the tool name and the outcome, and is not given the
// arguments at all — it cannot leak a brief because it never receives one.
func TestTheObserverSeesTheOutcomeAndNotTheArguments(t *testing.T) {
	var gotTool string
	var gotRefusal Refusal
	s := NewServer(Options{
		Caller:  &stubCaller{status: 403, resp: `{"error":"no"}`},
		Version: "test",
		Observe: func(_ context.Context, tool string, r Refusal, _ error) {
			gotTool, gotRefusal = tool, r
		},
	})
	callOn(t, s, "workgraph_file_work", map[string]any{"brief": "a secret brief nobody should log"})
	if gotTool != "workgraph_file_work" {
		t.Errorf("the observer saw tool %q", gotTool)
	}
	if gotRefusal != RefusalPolicy {
		t.Errorf("the observer saw refusal %d, want %d", gotRefusal, RefusalPolicy)
	}
	// The Observer signature is the guarantee: there is no argument parameter,
	// so no future edit can start logging one without changing the type and
	// every implementation of it.
	var _ Observer = func(context.Context, string, Refusal, error) {}
}

func TestToolListIsSerialisable(t *testing.T) {
	b, err := json.Marshal(map[string]any{"tools": Tools()})
	if err != nil {
		t.Fatalf("the tool list does not marshal: %v", err)
	}
	if !strings.Contains(string(b), "annotations") {
		t.Error("annotations are missing from the wire form; clients key on that name to decide whether to prompt")
	}
}

// --- helpers -------------------------------------------------------------

func byName(t *testing.T, name string) *sdk.Tool {
	t.Helper()
	for _, tl := range Tools() {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("%s is missing from the tool set", name)
	return nil
}

func text(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func call(t *testing.T, c Caller, tool string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	return callOn(t, NewServer(Options{Caller: c, Version: "test", HasShell: true}), tool, args)
}

// callOn drives a real client against a real server over an in-memory
// transport, so the schemas, the argument decoding and the handler all take
// part. A test that called the handler directly would not notice a schema that
// disagrees with the struct it decodes into.
func callOn(t *testing.T, s *sdk.Server, tool string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("connecting the server: %v", err)
	}
	defer ss.Close()

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connecting the client: %v", err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", tool, err)
	}
	return res
}
