package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// The MCP server (wg-p4h.10).
//
//	wg mcp
//
// A subcommand of the CLI rather than a separate binary, so it reads the same
// credentials file, speaks to the same API and cannot drift from it. Local
// stdio, which needs no new inbound path and no new trust boundary, and reaches
// Claude Code, Codex and anything else that spawns a local process — which is
// where the audience actually is. A remote server needs OAuth rather than a
// pasted token; that is wg-p4h.11, deferred deliberately.
//
// CURATED, NOT GENERATED, and this is the design decision worth arguing with.
// Exposing all 24 operations would give a model 24 tools and no judgement. The
// tool list IS the prompt — it is what the model reads before deciding — so a
// long one makes every decision worse, not more capable. These six are the ones
// the skill leads with. Everything else stays in the CLI, which an agent can
// still run.

// tool is one entry in the MCP tool list.
type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

// tools is the curated set. Each description says what the tool is FOR, not what
// it calls: a model choosing between tools is reading intent, not routes.
var tools = []tool{
	{
		Name: "workgraph_inbox",
		Description: "What needs this person's attention in Workgraph right now. " +
			"Use for 'what needs me', 'what's on my plate', 'anything waiting on me'.",
		InputSchema: obj(nil),
	},
	{
		Name: "workgraph_ask",
		Description: "Ask Workgraph's chief of staff a question about the portfolio. " +
			"It answers a FIXED set of questions; call with no question to list them, " +
			"then use one verbatim. Do not invent a question it does not answer.",
		InputSchema: obj(map[string]any{
			"question": str("One of the supported questions. Omit to list them."),
		}),
	},
	{
		Name: "workgraph_work_list",
		Description: "Work items with their status and what each has cost so far. " +
			"Use for 'what is in flight', 'what did that cost', bead status.",
		InputSchema: obj(nil),
	},
	{
		Name:        "workgraph_project_list",
		Description: "Projects this person can see.",
		InputSchema: obj(nil),
	},
	{
		Name: "workgraph_file_work",
		Description: "File a planning job from a brief, which produces beads. " +
			"SPENDS MONEY: an agent runs. Confirm with the person first. " +
			"The brief needs a checkable outcome, not 'make it better'.",
		InputSchema: obj(map[string]any{
			"brief": str("One paragraph describing the outcome, with acceptance criteria."),
		}, "brief"),
	},
	{
		Name: "workgraph_dispatch",
		Description: "Run one bead. SPENDS MONEY: an agent runs against it. " +
			"Confirm with the person before calling this for work they did not ask you to run.",
		InputSchema: obj(map[string]any{
			"bead": str("The bead id, for example wg-abc."),
		}, "bead"),
	},
}

// Deliberately absent, and each for a reason rather than an oversight:
//
//	approvals   no token can decide an approval at all, so a tool for it would
//	            be a tool that always fails
//	tokens      minting needs an interactive session; a tool would teach the
//	            model to attempt something it cannot do
//	node paths  the execution node's own surface, authorised by a different
//	            credential entirely

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func mcpServe() (int, error) {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 8<<20)
	out := json.NewEncoder(os.Stdout)

	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}

		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "workgraph", "version": "1"},
			}
		case "tools/list":
			resp.Result = map[string]any{"tools": tools}
		case "tools/call":
			resp.Result = callTool(req.Params)
		case "notifications/initialized", "notifications/cancelled":
			// Notifications carry no id and take no reply.
			continue
		case "ping":
			resp.Result = map[string]any{}
		default:
			resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
		}

		if err := out.Encode(resp); err != nil {
			return exitError, err
		}
	}
	return exitOK, in.Err()
}

// callTool runs one tool and returns an MCP content result.
//
// A failed call comes back as content with isError rather than a transport
// error, because the model needs to READ the refusal — "this token may not
// dispatch" is information it can act on, and a protocol-level error is not
// something it can reason about.
func callTool(params json.RawMessage) map[string]any {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return errorContent("could not read the tool call")
	}

	var (
		status int
		body   []byte
		err    error
	)
	switch p.Name {
	case "workgraph_inbox":
		status, body, err = do(http.MethodGet, "/v1/inbox", nil)
	case "workgraph_ask":
		q, _ := p.Arguments["question"].(string)
		path := "/v1/ask"
		if q != "" {
			path += "?q=" + urlEscape(q)
		}
		status, body, err = do(http.MethodGet, path, nil)
	case "workgraph_work_list":
		status, body, err = do(http.MethodGet, "/v1/work", nil)
	case "workgraph_project_list":
		status, body, err = do(http.MethodGet, "/v1/projects", nil)
	case "workgraph_file_work":
		brief, _ := p.Arguments["brief"].(string)
		if strings.TrimSpace(brief) == "" {
			return errorContent("a brief is required, and it needs a checkable outcome")
		}
		status, body, err = do(http.MethodPost, "/v1/work/plan", map[string]any{"brief": brief})
	case "workgraph_dispatch":
		bead, _ := p.Arguments["bead"].(string)
		if strings.TrimSpace(bead) == "" {
			return errorContent("a bead id is required")
		}
		status, body, err = do(http.MethodPost, "/v1/work/"+bead+"/dispatch", map[string]any{})
	default:
		return errorContent("unknown tool: " + p.Name)
	}

	if err != nil {
		return errorContent(err.Error())
	}
	if codeFor(status, body) != exitOK {
		// The server's own words. Rewording a refusal would make the model
		// reason about a paraphrase of what happened.
		return errorContent(refusalText(status, body))
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(body)}},
	}
}

func refusalText(status int, body []byte) string {
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(body, &e)
	msg := e.Error
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	if e.Code != "" {
		msg += " [" + e.Code + "]"
	}
	// Naming the retry position saves the model guessing at it.
	switch codeFor(status, body) {
	case exitForbidden, exitPolicyRefused:
		msg += " — do not retry; this needs a different credential or a person."
	case exitRateLimited:
		msg += " — rate limited; wait before retrying."
	case exitUnauthenticated:
		msg += " — the user must run `wg login`."
	}
	return msg
}

func errorContent(msg string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []map[string]any{{"type": "text", "text": msg}},
	}
}
