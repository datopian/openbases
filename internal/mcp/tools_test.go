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
//
// Raised from 8 to 9 on 2026-09-07, for project setup: creating a project and
// attaching repositories are what make work dispatchable at all, and doing
// them was the one part of the loop that still required a browser or a CLI.
//
// Nine because that is what those two tools cost, not because nine is a better
// number. The alternatives were both worse. Merging workgraph_bead into
// workgraph_work_list was considered and rejected — the comment above toolBead
// already argues that case, and it was written by somebody who had thought
// about it: the list is read to scan and the detail is read to understand one
// thing. Dropping a tool to make room would have removed something people use
// to add something they asked for.
//
// A tenth needs the same argument made again, in writing, here.
//
// Raised from 9 to 10 on 2026-09-10, for workgraph_job, and the argument is a
// reported dead end rather than a wish. workgraph_beads_file returns a job id
// and nothing could look that id up, so the only available signal was whether
// new beads had appeared -- which makes "still planning", "died silently" and
// "finished having produced nothing" the same observation. Someone spent 25
// minutes on that and then re-filed a reworded brief to find out, which costs
// money and answers nothing. A tool that returns an id no tool can resolve is
// an incomplete surface, not a small one.
//
// The alternatives were considered and are worse:
//
//	"Add to the CLI instead", which is what this test suggests, does not help
//	the person who hit it -- they work through MCP, and a CLI they cannot
//	reach closes nothing;
//
//	folding it into workgraph_bead breaks the Routes table, which this file
//	asserts is one route per tool, and conflates two subjects: a plan job has
//	no bead at all, so "tell me about this id" would answer about different
//	entities depending on the id's shape;
//
//	dropping a tool to make room would remove something people use.
//
// It earns its slot by being read-only, cheap, and answering in one sentence
// the question that otherwise costs a re-filed brief.
// Back to ten on 2026-09-10: workgraph_beads_file took the slot
// workgraph_file_work vacated. Planning moved out of the platform, so the
// tool that started an agent to invent a plan was replaced by the one that
// records a plan already made, and the transition is over.
func TestToolSetStaysSmall(t *testing.T) {
	if len(Tools()) > 10 {
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
	for _, name := range []string{"workgraph_dispatch"} {
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
		{"workgraph_bead", "GET", "/v1/work/sa-kfh", map[string]any{"bead": "sa-kfh"}},
		{"workgraph_project_list", "GET", "/v1/projects", nil},
		{"workgraph_beads_file", "POST", "/v1/work/beads", map[string]any{
			"project": "msf",
			"beads":   []any{map[string]any{"ref": "a", "title": "A"}},
		}},
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
// that forgets half the call does not file an empty plan.
func TestAnEmptyPlanIsRefusedBeforeCalling(t *testing.T) {
	stub := &stubCaller{status: 200, resp: `{}`}
	res := call(t, stub, "workgraph_beads_file", map[string]any{"project": "   "})
	if !res.IsError {
		t.Fatal("a plan with no project was accepted")
	}
	if stub.method != "" {
		t.Errorf("it called %s %s anyway", stub.method, stub.path)
	}
}

// The observer sees the tool name and the outcome, and is not given the
// arguments at all — it cannot leak a plan because it never receives one.
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
	callOn(t, s, "workgraph_beads_file", map[string]any{
		"project": "msf",
		"beads":   []any{map[string]any{"ref": "a", "title": "a secret title nobody should log"}},
	})
	if gotTool != "workgraph_beads_file" {
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

// The bead detail tool is what answers "did that work", so it must reach one
// bead rather than the list, and must escape the id into the path.
func TestTheBeadToolEscapesTheIdIntoThePath(t *testing.T) {
	stub := &stubCaller{status: 200, resp: `{}`}
	call(t, stub, "workgraph_bead", map[string]any{"bead": "sa-a/../../v1/tokens"})
	if strings.Contains(stub.path, "../") {
		t.Errorf("an unescaped bead id reached the path: %q", stub.path)
	}
	if !strings.HasPrefix(stub.path, "/v1/work/") {
		t.Errorf("the bead tool called %q, which is not one bead", stub.path)
	}
}

func TestTheBeadToolRefusesAnEmptyId(t *testing.T) {
	stub := &stubCaller{status: 200, resp: `{}`}
	res := call(t, stub, "workgraph_bead", map[string]any{"bead": "  "})
	if !res.IsError {
		t.Fatal("an empty bead id was accepted")
	}
	if stub.method != "" {
		t.Errorf("it called %s %s anyway", stub.method, stub.path)
	}
}

// A 404 from the detail endpoint means "no such bead, or none you can see",
// and the model must be able to read that rather than see a transport failure.
func TestAMissingBeadIsAReadableRefusal(t *testing.T) {
	stub := &stubCaller{status: 404, resp: `{"error":"no such bead, or none you can see","code":"not_found"}`}
	res := call(t, stub, "workgraph_bead", map[string]any{"bead": "sa-nope"})
	if !res.IsError {
		t.Error("a missing bead should be flagged isError")
	}
	if !strings.Contains(text(res), "no such bead") {
		t.Errorf("the server's words did not survive: %q", text(res))
	}
}

// Setting a project up is now possible from a chat client, which is the point:
// creating a project and attaching repositories are what make work
// dispatchable at all, and until now they were the one part of the loop that
// needed a browser or a CLI.
func TestAProjectCanBeSetUpFromAClient(t *testing.T) {
	t.Run("creating one reaches the right route with the right body", func(t *testing.T) {
		stub := &stubCaller{status: 201, resp: `{"slug":"jackson-open-data"}`}
		res := call(t, stub, "workgraph_project_create", map[string]any{
			"slug":          "jackson-open-data",
			"name":          "Jackson Open Data",
			"backup_owner":  "colleague@datopian.com",
			"primary_owner": "me@datopian.com",
			"portfolio":     "bizdev",
			"cell":          "oss",
		})
		if res.IsError {
			t.Fatalf("refused a complete request: %s", text(res))
		}
		if stub.method != http.MethodPost || stub.path != "/v1/projects" {
			t.Errorf("called %s %s", stub.method, stub.path)
		}
		body, ok := stub.body.(map[string]any)
		if !ok {
			t.Fatalf("body is %T", stub.body)
		}
		for field, want := range map[string]string{
			"slug": "jackson-open-data", "name": "Jackson Open Data",
			"backup_owner": "colleague@datopian.com", "primary_owner": "me@datopian.com",
			"portfolio": "bizdev", "cell": "oss",
		} {
			if body[field] != want {
				t.Errorf("body[%q] = %v, want %q", field, body[field], want)
			}
		}
		// Fields nobody gave are ABSENT rather than empty strings. An empty
		// visibility would be sent as "" and refused by the schema, which the
		// caller would read as their own mistake.
		if _, present := body["visibility"]; present {
			t.Errorf("visibility was sent though nobody set it: %v", body)
		}
		if _, present := body["objective"]; present {
			t.Errorf("objective was sent though nobody set it: %v", body)
		}
	})

	// The backup owner cannot be defaulted. The schema's backup_owner_differs
	// makes full-cycle ownership impossible to vest in one person, so a tool
	// that invented one would be deciding accountability by language model.
	//
	// Two layers refuse it, and which one speaks is worth knowing rather than
	// assuming. The SDK validates the generated JSON schema before the handler
	// runs, so a MISSING field is refused there — tersely, naming the property.
	// A field that is present but blank passes the schema and is refused by the
	// handler, which is the one that can explain.
	t.Run("a missing field is refused by the schema, naming it", func(t *testing.T) {
		for _, missing := range []string{"slug", "name", "backup_owner"} {
			args := map[string]any{
				"slug": "x", "name": "X", "backup_owner": "b@datopian.com",
			}
			delete(args, missing)
			stub := &stubCaller{status: 201, resp: `{}`}
			res := call(t, stub, "workgraph_project_create", args)
			if !res.IsError {
				t.Errorf("accepted a request with no %s", missing)
				continue
			}
			if !strings.Contains(text(res), missing) {
				t.Errorf("the refusal does not name %s: %s", missing, text(res))
			}
			if stub.method != "" {
				t.Errorf("called the API despite a missing %s", missing)
			}
		}
	})

	t.Run("a blank field is refused by the handler, which explains why", func(t *testing.T) {
		stub := &stubCaller{status: 201, resp: `{}`}
		res := call(t, stub, "workgraph_project_create", map[string]any{
			"slug": "x", "name": "X", "backup_owner": "   ",
		})
		if !res.IsError {
			t.Fatalf("accepted a blank backup owner: %s", text(res))
		}
		for _, want := range []string{"backup_owner", "different person", "ask"} {
			if !strings.Contains(text(res), want) {
				t.Errorf("the refusal does not say %q: %s", want, text(res))
			}
		}
		if stub.method != "" {
			t.Error("called the API with a blank backup owner")
		}
	})

	// And the explanation reaches the model BEFORE it calls, which is where it
	// actually changes behaviour: a model that reads "missing property
	// backup_owner" after the fact will invent an address, while one that read
	// the field description first will ask.
	t.Run("the field description tells the model to ask rather than choose", func(t *testing.T) {
		// Asked of a CONNECTED CLIENT, not of Tools(). The SDK generates the
		// input schema from the argument struct when the tool is registered,
		// so the declarations Tools() returns carry an empty schema and a test
		// reading them would assert nothing about what a model receives.
		schema := advertisedSchema(t, "workgraph_project_create")
		for _, want := range []string{"different person", "Ask the person", "REQUIRED"} {
			if !strings.Contains(schema, want) {
				t.Errorf("the schema a model reads does not say %q:\n%s", want, schema)
			}
		}
	})

	t.Run("attaching repositories sends them all in one call", func(t *testing.T) {
		stub := &stubCaller{status: 200, resp: `{"results":[]}`}
		res := call(t, stub, "workgraph_repositories_attach", map[string]any{
			"slug":         "portaljs",
			"repositories": []any{"datopian/portaljs", " datopian/cloud.portaljs.com ", ""},
		})
		if res.IsError {
			t.Fatalf("refused: %s", text(res))
		}
		if stub.path != "/v1/projects/portaljs/repositories" {
			t.Errorf("called %s", stub.path)
		}
		body := stub.body.(map[string]any)
		got, _ := body["repositories"].([]string)
		// Trimmed, and the empty one dropped rather than sent for the server
		// to refuse.
		if len(got) != 2 || got[0] != "datopian/portaljs" || got[1] != "datopian/cloud.portaljs.com" {
			t.Errorf("sent %v", got)
		}
	})

	t.Run("attaching nothing is refused before the call", func(t *testing.T) {
		for _, args := range []map[string]any{
			{"slug": "portaljs", "repositories": []any{}},
			{"slug": "portaljs", "repositories": []any{"  "}},
			{"slug": "", "repositories": []any{"datopian/portaljs"}},
		} {
			stub := &stubCaller{status: 200, resp: `{}`}
			if res := call(t, stub, "workgraph_repositories_attach", args); !res.IsError {
				t.Errorf("accepted %v", args)
			} else if stub.method != "" {
				t.Errorf("called the API for %v", args)
			}
		}
	})
}

// advertisedSchema is the input schema a connected client is given for a tool.
func advertisedSchema(t *testing.T, tool string) string {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	server := NewServer(Options{Caller: &stubCaller{status: 200, resp: "{}"}, Version: "test"})
	ss, err := server.Connect(ctx, st, nil)
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

	list, err := cs.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("listing tools: %v", err)
	}
	for _, tl := range list.Tools {
		if tl.Name != tool {
			continue
		}
		encoded, err := json.Marshal(tl.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	t.Fatalf("%s is not advertised to a client", tool)
	return ""
}
