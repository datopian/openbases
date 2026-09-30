// Package jev calls TypeSafe AI's Jev model through the AI Gateway.
//
// Jev is a "System One" model (ADR-0029): it does not generate text. It
// evaluates one STATE against many typed QUESTIONS in a single pass and returns
// calibrated, typed answers -- a truth value, a choice, or a score, each with a
// confidence. That makes it the cheap judgement engine the control plane has
// wanted: a verdict is a number to threshold on, not prose to parse, and at
// $0.042 per 1M input tokens it can run on every bead.
//
// This is the CONTROL-PLANE half of ADR-0018's two planes, exactly like
// internal/inference: the caller writes the questions, and the request is tagged
// and metered through the same gateway so its spend lands in the same table as
// every model call. Agent inference still goes through wg-runner.
//
// It reaches the gateway the same way internal/inference does -- cf-aig headers,
// payloads suppressed by default -- but Jev has its own request and response
// shape (state + typed questions), not OpenAI chat, so it is a separate client
// rather than a route on that one.
package jev

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

// QuestionType is one of Jev's three primitives.
type QuestionType string

const (
	// TypeNoul is a truth evaluation: "is this true of the state?" It answers a
	// float in [0,1] and carries no confidence -- the value IS the calibrated
	// probability.
	TypeNoul QuestionType = "noul"
	// TypeChoice selects one option. It answers the chosen key plus a
	// probability per option and a confidence.
	TypeChoice QuestionType = "choice"
	// TypeScore rates on a rubric. It answers a number (which may be fractional
	// between rubric points) plus probabilities and a confidence.
	TypeScore QuestionType = "score"
)

// Question is one atomic judgement against the shared state.
//
// Key is the caller's own name for it; the answer comes back under that key.
// Keep each question atomic -- "the kind of judgement a knowledgeable person
// could make in a few seconds" -- and combine several with application logic
// rather than asking one broad question.
type Question struct {
	Key          string
	Type         QuestionType
	Instructions string
	// Criteria is encoded as Jev expects for the type: for a choice, an
	// option->description map; for a score, an ordered list of rubric labels;
	// nil for a noul. Left as any so each constructor supplies the right shape
	// and nothing else has to know it.
	Criteria any
}

// Noul builds a truth question.
func Noul(key, instructions string) Question {
	return Question{Key: key, Type: TypeNoul, Instructions: instructions}
}

// Choice builds a selection question from option->description pairs.
func Choice(key, instructions string, options map[string]string) Question {
	return Question{Key: key, Type: TypeChoice, Instructions: instructions, Criteria: options}
}

// Score builds a rubric question from ordered labels (lowest first).
func Score(key, instructions string, labels []string) Question {
	return Question{Key: key, Type: TypeScore, Instructions: instructions, Criteria: labels}
}

// Metadata is what the spend is attributed to, mirroring internal/inference so
// control-plane judgement and agent runs land in one table and compare.
//
// The gateway drops metadata past five keys silently, so this is a fixed struct
// rather than a map: a sixth field would not fail, it would quietly stop one of
// these arriving.
type Metadata struct {
	Cell string
	Bead string
	Rig  string
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
	b, err := json.Marshal(fields)
	return string(b), err
}

// Request is one evaluation: a state and the questions to ask of it.
type Request struct {
	// State is the content to evaluate. Any JSON-encodable value -- a string, or
	// a struct/map the model reads fields off. Jev's context is 32k tokens, so a
	// caller summarises; this client does not.
	State any
	// Questions are evaluated in parallel and in isolation against the same
	// state. Adding questions barely changes the response time.
	Questions []Question
	Metadata  Metadata
}

// Answer is one question's typed result. Which fields are set depends on the
// question type: Noul sets Noul; Choice sets Choice, Probabilities, Confidence;
// Score sets Score, Probabilities, Confidence. The pointers distinguish "zero"
// from "absent".
type Answer struct {
	Noul          *float64
	Choice        string
	Score         *float64
	Probabilities map[string]float64
	Confidence    *float64
}

// Response is the answers, keyed by Question.Key, and the model that produced
// them (a route or fallback resolves to a versioned id worth recording).
type Response struct {
	Answers map[string]Answer
	Model   string
}

// Client calls one gateway's Jev model.
type Client struct {
	// BaseURL is the gateway prefix WITHOUT a provider segment, e.g.
	// https://gateway.ai.cloudflare.com/v1/<account>/workgraph-staging-oss --
	// the same value internal/inference takes.
	BaseURL string
	// Token authorises the request through the gateway (cf-aig-authorization).
	// Without it the request would reach the provider directly, untagged,
	// unmetered and outside every budget -- so it is refused here.
	Token string
	HTTP  *http.Client
	// ProviderPath is where the model lives under the gateway. Jev is a
	// Workers-AI-catalogued model (typesafe/jev); kept configurable so the exact
	// path can be corrected without a code change.
	ProviderPath string
	// CollectPayloads stores the state and answers in the gateway's shared log.
	// The zero value does NOT, deliberately: that log is one store shared across
	// nine gateways (see internal/inference), and a state can carry a diff.
	CollectPayloads bool
}

// DefaultProviderPath is the gateway path for the Jev model.
const DefaultProviderPath = "/workers-ai/run/typesafe/jev"

// New builds a client with a sane timeout and the default provider path.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL:      strings.TrimSuffix(baseURL, "/"),
		Token:        token,
		ProviderPath: DefaultProviderPath,
		HTTP:         &http.Client{Timeout: 30 * time.Second},
	}
}

// Evaluate asks the questions of the state and returns the typed answers.
func (c *Client) Evaluate(ctx context.Context, r Request) (Response, error) {
	var out Response
	if c.BaseURL == "" {
		return out, fmt.Errorf("no gateway base URL")
	}
	if c.Token == "" {
		return out, fmt.Errorf("no gateway token; the request would reach the provider untagged and outside every budget")
	}
	if len(r.Questions) == 0 {
		return out, fmt.Errorf("a request needs at least one question")
	}

	questions := make(map[string]any, len(r.Questions))
	for _, q := range r.Questions {
		if strings.TrimSpace(q.Key) == "" {
			return out, fmt.Errorf("every question needs a key; the answer comes back under it")
		}
		if _, dup := questions[q.Key]; dup {
			return out, fmt.Errorf("two questions share the key %q; answers are keyed by it", q.Key)
		}
		entry := map[string]any{"type": string(q.Type), "instructions": q.Instructions}
		if q.Criteria != nil {
			entry["criteria"] = q.Criteria
		}
		questions[q.Key] = entry
	}

	body, err := json.Marshal(map[string]any{"state": r.State, "questions": questions})
	if err != nil {
		return out, err
	}
	meta, err := r.Metadata.headerValue()
	if err != nil {
		return out, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+c.ProviderPath, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("cf-aig-authorization", "Bearer "+c.Token)
	req.Header.Set("cf-aig-metadata", meta)
	if !c.CollectPayloads {
		// Suppresses the request and response BODIES only, keeping the metadata,
		// cost and model -- the same lever internal/inference documents.
		req.Header.Set("cf-aig-collect-log-payload", "false")
	}
	req.Header.Set("Content-Type", "application/json")
	// Named: Cloudflare answers an unnamed client with a 403/1010 bot challenge
	// that reads exactly like an auth failure.
	req.Header.Set("User-Agent", "workgraph-jev/1")

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
		return out, ErrOverBudget{Body: truncate(string(raw), 200)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("gateway returned %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return parse(raw)
}

// parse decodes Jev's answers, tolerant of the envelope.
//
// Workers-AI responses usually wrap the payload in {"result": ..., "success":
// true}; a direct call does not. And the answers may sit under an "answers"
// object or beside "model"/"usage" at the top of the payload. Rather than pin a
// shape we have not yet seen live (ADR-0029 calls for validating this against a
// real response when the gate is wired in), this accepts all of those.
func parse(raw []byte) (Response, error) {
	out := Response{Answers: map[string]Answer{}}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return out, fmt.Errorf("jev returned something that is not an object: %w", err)
	}
	// Descend into result if present.
	if r, ok := envelope["result"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(r, &inner); err == nil {
			envelope = inner
		}
	}
	if m, ok := envelope["model"]; ok {
		_ = json.Unmarshal(m, &out.Model)
	}
	// Answers: an explicit "answers" object, or every remaining key.
	answers := envelope
	if a, ok := envelope["answers"]; ok {
		answers = map[string]json.RawMessage{}
		if err := json.Unmarshal(a, &answers); err != nil {
			return out, fmt.Errorf("jev answers were not an object: %w", err)
		}
	}
	reserved := map[string]bool{"model": true, "usage": true, "success": true, "errors": true, "answers": true}
	for key, rawAns := range answers {
		if reserved[key] {
			continue
		}
		var a struct {
			Noul          *float64           `json:"noul"`
			Choice        string             `json:"choice"`
			Score         *float64           `json:"score"`
			Probabilities map[string]float64 `json:"probabilities"`
			Confidence    *float64           `json:"confidence"`
		}
		if err := json.Unmarshal(rawAns, &a); err != nil {
			// A non-answer field that slipped through; skip rather than fail the
			// whole evaluation over one unexpected key.
			continue
		}
		out.Answers[key] = Answer(a)
	}
	if len(out.Answers) == 0 {
		return out, fmt.Errorf("jev returned no answers: %s", truncate(string(raw), 200))
	}
	return out, nil
}

// ErrOverBudget is a 429 from a budget node or a gateway spend limit -- an
// operational event to raise and stop on, not an error to retry into.
type ErrOverBudget struct{ Body string }

func (e ErrOverBudget) Error() string { return "over budget at the gateway: " + e.Body }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
