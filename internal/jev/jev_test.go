package jev

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A real round-trip against a fake gateway: the request Jev receives is exactly
// the state-plus-typed-questions shape, carries the cf-aig headers, and the
// typed answers are parsed back under the caller's keys. A fake rather than the
// live model because what is asserted is this client's wire behaviour; the live
// response envelope is validated when the gate is wired in (ADR-0029).
func TestEvaluateSendsTypedQuestionsAndParsesTypedAnswers(t *testing.T) {
	var gotPath, gotAuth, gotMeta, gotPayloadHdr, gotUA string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("cf-aig-authorization")
		gotMeta = r.Header.Get("cf-aig-metadata")
		gotPayloadHdr = r.Header.Get("cf-aig-collect-log-payload")
		gotUA = r.Header.Get("User-Agent")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		// A workers-ai-style wrapped response covering all three answer types.
		io.WriteString(w, `{"success":true,"result":{"model":"jev-2026-09",
			"answers":{
				"dept":{"choice":"billing","probabilities":{"billing":0.8,"technical":0.2},"confidence":0.91},
				"anger":{"score":1.2,"probabilities":{"0":0.1,"1":0.7,"2":0.2},"confidence":0.66},
				"refund":{"noul":0.95}
			}}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "tok-123")
	resp, err := c.Evaluate(context.Background(), Request{
		State: map[string]any{"ticket": "charged twice"},
		Questions: []Question{
			Choice("dept", "which team", map[string]string{"billing": "payments", "technical": "bugs"}),
			Score("anger", "how frustrated", []string{"calm", "civil", "angry"}),
			Noul("refund", "the customer asks for a refund"),
		},
		Metadata: Metadata{Cell: "oss", Bead: "wg-1"},
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// The request reached the model path with the gateway headers.
	if !strings.HasSuffix(gotPath, "/workers-ai/typesafe/jev") {
		t.Errorf("path = %q, want it to end with the jev model path", gotPath)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("cf-aig-authorization = %q", gotAuth)
	}
	if gotPayloadHdr != "false" {
		t.Errorf("payloads should be suppressed by default, header = %q", gotPayloadHdr)
	}
	if gotUA != "workgraph-jev/1" {
		t.Errorf("User-Agent = %q; an unnamed client gets a 403/1010", gotUA)
	}
	if !strings.Contains(gotMeta, `"role":"control-plane"`) || !strings.Contains(gotMeta, `"bead":"wg-1"`) {
		t.Errorf("metadata = %q, want role and bead", gotMeta)
	}

	// The body is state + questions keyed by our keys, each with its type.
	if _, ok := gotBody["state"]; !ok {
		t.Error("body has no state")
	}
	qs, ok := gotBody["questions"].(map[string]any)
	if !ok {
		t.Fatalf("body questions is not an object: %T", gotBody["questions"])
	}
	dept, ok := qs["dept"].(map[string]any)
	if !ok || dept["type"] != "choice" {
		t.Errorf("dept question = %v, want type choice", qs["dept"])
	}
	if _, ok := dept["criteria"]; !ok {
		t.Error("choice question dropped its criteria")
	}
	if refund, _ := qs["refund"].(map[string]any); refund["type"] != "noul" {
		t.Errorf("refund question type = %v, want noul", refund["type"])
	}

	// The answers parsed back, typed, under the caller's keys.
	if resp.Model != "jev-2026-09" {
		t.Errorf("model = %q", resp.Model)
	}
	if got := resp.Answers["dept"]; got.Choice != "billing" || got.Confidence == nil || *got.Confidence != 0.91 {
		t.Errorf("dept answer = %+v", got)
	}
	if got := resp.Answers["dept"].Probabilities["billing"]; got != 0.8 {
		t.Errorf("dept billing probability = %v", got)
	}
	if got := resp.Answers["anger"]; got.Score == nil || *got.Score != 1.2 {
		t.Errorf("anger score = %+v", got)
	}
	if got := resp.Answers["refund"]; got.Noul == nil || *got.Noul != 0.95 {
		t.Errorf("refund noul = %+v", got)
	}
	if resp.Answers["refund"].Confidence != nil {
		t.Error("a noul answer should carry no confidence")
	}
}

// A bare (unwrapped) response, answers flat beside model, is parsed too.
func TestEvaluateParsesUnwrappedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"jev-x","done":{"noul":0.4}}`)
	}))
	defer srv.Close()
	resp, err := c(srv).Evaluate(context.Background(), Request{
		Questions: []Question{Noul("done", "is it done")},
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if resp.Model != "jev-x" {
		t.Errorf("model = %q", resp.Model)
	}
	if a := resp.Answers["done"]; a.Noul == nil || *a.Noul != 0.4 {
		t.Errorf("done = %+v", a)
	}
}

func TestEvaluateRefusesMissingToken(t *testing.T) {
	cl := &Client{BaseURL: "https://x", ProviderPath: DefaultProviderPath}
	if _, err := cl.Evaluate(context.Background(), Request{Questions: []Question{Noul("k", "q")}}); err == nil {
		t.Fatal("a call with no token was allowed; it would run untagged and outside every budget")
	}
}

func TestEvaluateRefusesNoQuestions(t *testing.T) {
	if _, err := New("https://x", "t").Evaluate(context.Background(), Request{}); err == nil {
		t.Fatal("a call with no questions was allowed")
	}
}

func TestEvaluateRefusesDuplicateKeys(t *testing.T) {
	_, err := New("https://x", "t").Evaluate(context.Background(), Request{
		Questions: []Question{Noul("k", "a"), Noul("k", "b")},
	})
	if err == nil {
		t.Fatal("duplicate keys were allowed; answers are keyed by them")
	}
}

func TestOverBudgetIsItsOwnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "spend limit")
	}))
	defer srv.Close()
	_, err := c(srv).Evaluate(context.Background(), Request{Questions: []Question{Noul("k", "q")}})
	var ob ErrOverBudget
	if !errors.As(err, &ob) {
		t.Fatalf("a 429 should be ErrOverBudget, got %v", err)
	}
}

func c(srv *httptest.Server) *Client { return New(srv.URL, "tok") }
