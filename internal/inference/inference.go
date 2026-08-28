// Package inference calls Workgraph-authored inference through the AI Gateway.
//
// This is the CONTROL-PLANE half of ADR-0018's two planes: extraction,
// classification, ranking, summarisation — work we write the callers for.
// Agent inference goes through wg-runner instead, because Claude Code and
// OpenCode speak their own protocols and cannot address a route (ADR-0024).
//
// A caller names a TASK CLASS, never a model:
//
//	out, err := c.Complete(ctx, inference.Request{
//	        Route: inference.RouteClassify,
//	        Prompt: "...",
//	        Metadata: inference.Metadata{Cell: "oss", Bead: "wg-1a2"},
//	})
//
// Which model that reaches, what it falls back to when the model fails, and the
// ceiling it spends under are all in infra/gateway/routes/<name>.json. Changing
// any of them is a change to that file and a script run — not a code change, and
// not a change to every caller (wg-sn4).
package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Route names a task class. The value is the route in infra/gateway/routes.
type Route string

const (
	// RouteClassify is T0: classification, routing, tagging, extraction, short
	// operational text. Work with one right answer and no deliberation.
	RouteClassify Route = "wg-classify"
)

// Metadata is what the spend is attributed to.
//
// The same four keys the agent runner sends, so control-plane inference and
// agent runs land in one table and can be compared. The gateway drops metadata
// beyond five keys SILENTLY, so this is a struct rather than a map: a sixth
// field would not fail, it would quietly stop one of these arriving.
type Metadata struct {
	// Cell attributes the spend, and is the key the route's budget node counts
	// against. Empty means control-plane work that belongs to no cell.
	Cell string
	// Bead is the work this was done for, if any.
	Bead string
	// Rig narrows it further within a cell.
	Rig string
}

func (m Metadata) headerValue() (string, error) {
	fields := map[string]string{"role": "control-plane"}
	if m.Cell != "" {
		fields["cell"] = m.Cell
	}
	if m.Bead != "" {
		fields["bead"] = m.Bead
	}
	if m.Rig != "" {
		fields["rig"] = m.Rig
	}
	encoded, err := json.Marshal(fields)
	return string(encoded), err
}

// Request is one call.
type Request struct {
	Route  Route
	Prompt string
	// System is optional. Kept separate from Prompt so a caller can hold it
	// stable across calls, which is what makes a cached prefix possible later.
	System string
	// MaxTokens bounds the answer. Required, because the failure it prevents is
	// not a large bill but an empty response: a reasoning model at a small
	// budget spends it thinking and returns nothing at all (ADR-0018).
	MaxTokens int
	Metadata  Metadata
}

// Response is what came back, and what produced it.
type Response struct {
	// Text is the answer. Never nil-checked into "" by this package — see the
	// note in Complete about content being null.
	Text string
	// Model is what the route actually resolved to. Worth surfacing: a route
	// that fell through to its fallback answers successfully, and the only sign
	// is this field.
	Model string
}

// Client calls one gateway.
type Client struct {
	// BaseURL is the gateway prefix without a provider segment, e.g.
	// https://gateway.ai.cloudflare.com/v1/<account>/workgraph-staging-oss
	BaseURL string
	// Token authorises the request through the gateway. Without it the request
	// does not fail — it reaches the provider directly, untagged, unmetered and
	// outside every budget.
	Token string
	HTTP  *http.Client
}

// New builds a client with a sane timeout.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Token:   token,
		// Longer than any T0 call should need and shorter than a caller's
		// patience. The route has its own per-model timeouts; this bounds the
		// whole thing including a fallback hop.
		HTTP: &http.Client{Timeout: 90 * time.Second},
	}
}

// Complete sends one request through the route and returns the answer.
func (c *Client) Complete(ctx context.Context, r Request) (Response, error) {
	var out Response
	if c.BaseURL == "" {
		return out, fmt.Errorf("no gateway base URL")
	}
	if c.Token == "" {
		return out, fmt.Errorf("no gateway token; the request would reach the provider untagged and outside every budget")
	}
	if r.Route == "" {
		return out, fmt.Errorf("a request needs a route; callers name a task class, not a model")
	}
	if strings.TrimSpace(r.Prompt) == "" {
		return out, fmt.Errorf("a request needs a prompt")
	}
	if r.MaxTokens <= 0 {
		return out, fmt.Errorf("a request needs MaxTokens; an unbounded answer is not the risk, an empty one is")
	}

	messages := []map[string]string{}
	if r.System != "" {
		messages = append(messages, map[string]string{"role": "system", "content": r.System})
	}
	messages = append(messages, map[string]string{"role": "user", "content": r.Prompt})

	body, err := json.Marshal(map[string]any{
		// dynamic/<route>, which is what makes this a routing decision rather
		// than a model choice.
		"model":      "dynamic/" + string(r.Route),
		"messages":   messages,
		"max_tokens": r.MaxTokens,
	})
	if err != nil {
		return out, err
	}

	meta, err := r.Metadata.headerValue()
	if err != nil {
		return out, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/compat/chat/completions", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("cf-aig-authorization", "Bearer "+c.Token)
	req.Header.Set("cf-aig-metadata", meta)
	req.Header.Set("Content-Type", "application/json")
	// Named. Cloudflare answers an unnamed client with a 403 and error code
	// 1010, which is a bot challenge and reads exactly like an auth failure.
	req.Header.Set("User-Agent", "workgraph-inference/1")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return out, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// The route's budget node, or a gateway spend limit. An operational
		// event, not an error to retry into (ADR-0018): the caller should raise
		// an attention item and stop.
		return out, ErrOverBudget{Body: truncate(string(raw), 200)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("gateway returned %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var decoded struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				Reasoning *string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return out, fmt.Errorf("gateway returned something that is not a completion: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return out, fmt.Errorf("gateway returned no choices")
	}

	out.Model = decoded.Model
	msg := decoded.Choices[0].Message
	switch {
	case msg.Content != nil && strings.TrimSpace(*msg.Content) != "":
		out.Text = *msg.Content
	case msg.Reasoning != nil && strings.TrimSpace(*msg.Reasoning) != "":
		// Required, not defensive. GLM-4.7-Flash returns its answer in
		// `reasoning` with `content` null, so a client reading only content
		// sees an empty string and NO error — and silently classifies
		// everything as blank. ADR-0018 records this as a client requirement
		// regardless of which model is default, because the default changes and
		// a route makes it change without a code review.
		out.Text = *msg.Reasoning
	default:
		return out, fmt.Errorf("model %s returned an empty answer; "+
			"if it is a reasoning model, MaxTokens (%d) may be too small to finish",
			decoded.Model, r.MaxTokens)
	}
	return out, nil
}

// ErrOverBudget is a 429 from a budget node or a gateway spend limit.
//
// Its own type because the correct response is different in kind: not a retry,
// which walks straight back into the limit, but stopping and telling somebody.
type ErrOverBudget struct{ Body string }

func (e ErrOverBudget) Error() string {
	return "over budget at the gateway: " + e.Body
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
