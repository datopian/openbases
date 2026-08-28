package inference

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, status int, body string, capture *http.Request) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			b, _ := io.ReadAll(r.Body)
			*capture = *r.Clone(context.Background())
			capture.Body = io.NopCloser(strings.NewReader(string(b)))
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok")
	c.HTTP = srv.Client()
	return c
}

func req() Request {
	return Request{Route: RouteClassify, Prompt: "classify this", MaxTokens: 16,
		Metadata: Metadata{Cell: "oss", Bead: "wg-1a2"}}
}

// A caller names a task class. What that reaches is a routing decision in a
// file, which is the whole point of wg-sn4.
func TestTheCallerNamesARouteNotAModel(t *testing.T) {
	var got http.Request
	c := serve(t, 200, `{"model":"@cf/meta/llama-3.3-70b-instruct-fp8-fast",
	  "choices":[{"message":{"content":"bug"}}]}`, &got)
	out, err := c.Complete(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(got.Body)
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "dynamic/wg-classify" {
		t.Errorf("model = %v, want dynamic/wg-classify", sent["model"])
	}
	// The model that answered is surfaced: a route that fell through to its
	// fallback succeeds, and this field is the only sign.
	if out.Model != "@cf/meta/llama-3.3-70b-instruct-fp8-fast" {
		t.Errorf("Model = %q, want the model that answered", out.Model)
	}
}

// Both headers or the run escapes its budget. The credential and the
// attribution are one variable.
func TestAttributionAndCredentialTravelTogether(t *testing.T) {
	var got http.Request
	c := serve(t, 200, `{"model":"m","choices":[{"message":{"content":"x"}}]}`, &got)
	if _, err := c.Complete(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("cf-aig-authorization") == "" {
		t.Error("no gateway credential")
	}
	meta := got.Header.Get("cf-aig-metadata")
	for _, want := range []string{`"cell":"oss"`, `"bead":"wg-1a2"`, `"role":"control-plane"`} {
		if !strings.Contains(meta, want) {
			t.Errorf("metadata %q is missing %s", meta, want)
		}
	}
}

// GLM-4.7-Flash returns its answer in `reasoning` with `content` null. A client
// reading only content sees an empty string and no error, and silently
// classifies everything as blank. A route makes the model change without a code
// review, so this cannot be left to whichever model is default today.
func TestAnAnswerInReasoningIsNotLostWhenContentIsNull(t *testing.T) {
	c := serve(t, 200, `{"model":"@cf/zai-org/glm-4.7-flash",
	  "choices":[{"message":{"content":null,"reasoning":"chore"}}]}`, nil)
	out, err := c.Complete(context.Background(), req())
	if err != nil {
		t.Fatalf("an answer in reasoning was treated as a failure: %v", err)
	}
	if out.Text != "chore" {
		t.Errorf("Text = %q, want chore", out.Text)
	}
}

// An empty answer is an error, not an empty string. Returning "" would be
// indistinguishable from a model that classified everything as nothing.
func TestAnEmptyAnswerIsAnError(t *testing.T) {
	c := serve(t, 200, `{"model":"m","choices":[{"message":{"content":null}}]}`, nil)
	_, err := c.Complete(context.Background(), req())
	if err == nil {
		t.Fatal("an empty answer was returned as success")
	}
	if !strings.Contains(err.Error(), "MaxTokens") {
		t.Errorf("the error should point at the likely cause: %v", err)
	}
}

// A 429 is an operational event, not something to retry into. Its own type so a
// caller can stop and raise attention rather than walking back into the limit.
func TestOverBudgetHasItsOwnType(t *testing.T) {
	c := serve(t, 429, `{"error":"spend limit"}`, nil)
	_, err := c.Complete(context.Background(), req())
	if _, ok := err.(ErrOverBudget); !ok {
		t.Fatalf("a 429 came back as %T, which a caller would retry: %v", err, err)
	}
}

// Without the gateway a request does not fail, it succeeds against the provider
// directly — untagged, unmetered and outside every budget.
func TestARequestWithoutTheGatewayTokenIsRefused(t *testing.T) {
	c := New("https://example.invalid", "")
	if _, err := c.Complete(context.Background(), req()); err == nil {
		t.Error("a request with no gateway token was allowed")
	}
}

// A reasoning model at a small budget spends it thinking and returns nothing,
// so the bound is required rather than defaulted.
func TestMaxTokensIsRequired(t *testing.T) {
	c := serve(t, 200, `{}`, nil)
	r := req()
	r.MaxTokens = 0
	if _, err := c.Complete(context.Background(), r); err == nil {
		t.Error("an unbounded request was allowed")
	}
}
