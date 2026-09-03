// Package mcp is Workgraph's Model Context Protocol tool set, shared by both
// transports (wg-p4h.11).
//
// One definition, two ways in. `wg mcp` speaks stdio, for Claude Code and Codex
// on a laptop; control-api serves the same tools over Streamable HTTP at /mcp,
// for the clients that cannot spawn a local process — Claude Cowork's sandbox,
// claude.ai in a browser, and the phone apps. Those three are where the people
// actually are, and none of them can install a binary or hold an environment
// variable.
//
// The tools lived in cmd/wg/mcp.go and are here so the two transports cannot
// drift. A remote tool set that lags the local one by a release is worse than
// no remote tool set: the model reads the tool list as its prompt, and a stale
// prompt is a confidently wrong one.
//
// CURATED, NOT GENERATED, which is the decision worth arguing with and is
// carried over unchanged from ADR-0025. Exposing all the API's operations would
// give a model dozens of tools and no judgement. The tool list IS the prompt —
// it is what the model reads before deciding — so a long one makes every
// decision worse rather than more capable. Six tools, and the omissions are
// deliberate:
//
//	approvals   no credential may decide an approval, so a tool for it would be
//	            a tool that always fails
//	tokens      minting needs an interactive session; a tool would teach the
//	            model to attempt what it cannot do
//	node paths  the execution node's own surface, authorised by a different
//	            credential entirely
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Caller performs one Workgraph API operation and returns the raw answer.
//
// The two transports supply different implementations and neither is a special
// case of the other. `wg` calls the API over HTTPS with a personal token;
// control-api dispatches into its own authenticated mux in process, with the
// caller's Access identity already attached — no request leaves the node, so a
// tool call does not pay for a round trip through Cloudflare and back, and
// cannot be refused by Access for lacking a cookie it never had.
//
// Returning (status, body) rather than a decoded type is deliberate. The
// refusal a model needs to read is the SERVER'S OWN WORDS, and every layer that
// re-describes an error makes the model reason about a paraphrase.
type Caller interface {
	Call(ctx context.Context, method, path string, body any) (status int, respBody []byte, err error)
}

// Refusal is how a tool call failed, in the same terms as the CLI's exit codes.
//
// The table in skills/workgraph/SKILL.md is the contract an agent has already
// been taught, so the semantics are identical here and only the carrier
// changes: an exit code becomes a tool result the model can read.
type Refusal int

const (
	// RefusalNone means the call succeeded.
	RefusalNone Refusal = 0
	// RefusalOther is a fault rather than a decision: a 5xx, a timeout, a
	// malformed answer. Worth retrying once.
	RefusalOther Refusal = 1
	// RefusalUnauthenticated is exit 3: no usable credential.
	RefusalUnauthenticated Refusal = 3
	// RefusalForbidden is exit 4: this credential or role may not.
	RefusalForbidden Refusal = 4
	// RefusalPolicy is exit 5: refused by policy or budget. A normal answer
	// the model can act on, not a transport failure.
	RefusalPolicy Refusal = 5
	// RefusalRateLimited is exit 6.
	RefusalRateLimited Refusal = 6
	// RefusalConflict is exit 7.
	RefusalConflict Refusal = 7
)

// Classify maps an HTTP answer to a refusal, matching cmd/wg's codeFor exactly.
//
// 403 is two different problems wearing one status, and they have different
// fixes: a scope or role refusal needs a different credential, and a policy
// refusal needs a person. Telling them apart is the whole reason the CLI has
// two exit codes for it.
func Classify(status int, body []byte) Refusal {
	switch status {
	case 401:
		return RefusalUnauthenticated
	case 429:
		return RefusalRateLimited
	case 409:
		return RefusalConflict
	case 403:
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &e)
		switch e.Code {
		case "token_scope_insufficient", "role_grant_missing", "token_self_service_forbidden":
			return RefusalForbidden
		default:
			return RefusalPolicy
		}
	}
	if status >= 200 && status < 300 {
		return RefusalNone
	}
	return RefusalOther
}

// advice is what the model should do about a refusal, in words rather than a
// number. Naming the retry position saves it guessing, and guessing here means
// either a retry storm or giving up on something that would have worked.
func advice(r Refusal, hasShell bool) string {
	switch r {
	case RefusalForbidden, RefusalPolicy:
		return " — do not retry; this needs a different credential or a person."
	case RefusalRateLimited:
		return " — rate limited; wait before retrying."
	case RefusalUnauthenticated:
		if hasShell {
			return " — the user must run `wg login`."
		}
		// There is no login command on the connector path. The client asks the
		// person to authorise, so telling the model to run one would send it
		// looking for a shell it does not have.
		return " — the connector's authorisation has expired or was revoked;" +
			" the person must reconnect it in their client's settings."
	}
	return ""
}

// refusalText is the server's own message, plus what to do about it.
func refusalText(status int, body []byte, hasShell bool) string {
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(body, &e)
	msg := e.Error
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	if msg == "" {
		msg = fmt.Sprintf("the request failed with status %d", status)
	}
	if e.Code != "" {
		msg += " [" + e.Code + "]"
	}
	return msg + advice(Classify(status, body), hasShell)
}

func textResult(s string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}
}

// errorResult is a refusal the model reads rather than a transport error.
//
// IsError, not a returned Go error: the model needs to READ "this credential
// may not dispatch" and decide what to do. A protocol-level error is not
// something it can reason about, and a budget refusal in particular is a
// perfectly normal answer.
func errorResult(s string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: s}},
	}
}

func ptr[T any](v T) *T { return &v }
