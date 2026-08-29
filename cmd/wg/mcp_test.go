package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The tool list IS the prompt — it is what a model reads before deciding — so
// these assertions are about what the model will be told, not about plumbing.

// Curated, not generated. Twenty-four tools would give a model no judgement, and
// this test is the thing that stops "expose everything" happening by increment.
func TestToolSetStaysSmall(t *testing.T) {
	if len(tools) > 8 {
		t.Fatalf("%d tools. The list is the prompt: past a handful, a model chooses worse "+
			"rather than doing more. Add to the CLI instead.", len(tools))
	}
}

// A tool that always fails teaches the model to keep trying it.
func TestNoToolExistsForSomethingATokenCanNeverDo(t *testing.T) {
	for _, tl := range tools {
		n := strings.ToLower(tl.Name)
		switch {
		case strings.Contains(n, "approv"):
			t.Errorf("%s: no token can decide an approval, so this tool would always fail", tl.Name)
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
		found := false
		for _, tl := range tools {
			if tl.Name != name {
				continue
			}
			found = true
			if !strings.Contains(strings.ToUpper(tl.Description), "SPENDS MONEY") {
				t.Errorf("%s does not say it spends money; the model has no other way to know", name)
			}
			if !strings.Contains(strings.ToLower(tl.Description), "confirm") {
				t.Errorf("%s does not tell the model to confirm first", name)
			}
		}
		if !found {
			t.Errorf("%s is missing from the tool set", name)
		}
	}
}

// The ask tool must tell the model the question set is fixed, or it will invent
// a fifth question and collect 400s.
func TestAskToolSaysTheQuestionSetIsFixed(t *testing.T) {
	for _, tl := range tools {
		if tl.Name != "workgraph_ask" {
			continue
		}
		d := strings.ToLower(tl.Description)
		if !strings.Contains(d, "fixed") || !strings.Contains(d, "list them") {
			t.Error("workgraph_ask does not tell the model to list the questions rather than invent one")
		}
		return
	}
	t.Fatal("workgraph_ask is missing")
}

func TestEveryToolHasANameDescriptionAndSchema(t *testing.T) {
	seen := map[string]bool{}
	for _, tl := range tools {
		if tl.Name == "" || tl.Description == "" {
			t.Errorf("%+v is missing a name or description", tl)
		}
		if seen[tl.Name] {
			t.Errorf("duplicate tool name %q", tl.Name)
		}
		seen[tl.Name] = true
		if tl.InputSchema == nil || tl.InputSchema["type"] != "object" {
			t.Errorf("%s has no object input schema", tl.Name)
		}
		if !strings.HasPrefix(tl.Name, "workgraph_") {
			t.Errorf("%s is not namespaced; a model with several servers loaded needs to tell them apart", tl.Name)
		}
	}
}

// A refusal must reach the model as readable content, not a transport error.
// "This token may not dispatch" is information it can act on; a JSON-RPC error
// code is not something it can reason about.
func TestRefusalsComeBackAsReadableContent(t *testing.T) {
	out := errorContent("nope")
	if out["isError"] != true {
		t.Error("a refusal is not flagged isError")
	}
	content, ok := out["content"].([]map[string]any)
	if !ok || len(content) == 0 || content[0]["text"] != "nope" {
		t.Errorf("the refusal text did not survive: %+v", out)
	}
}

// The model should be told where it stands on retrying, rather than guessing.
func TestRefusalTextNamesTheRetryPosition(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body, want string
	}{
		{403, `{"error":"nope","code":"token_scope_insufficient"}`, "do not retry"},
		{429, `{"error":"slow down","code":"rate_limited"}`, "wait before retrying"},
		{401, `{"error":"unauthorized"}`, "wg login"},
	} {
		got := refusalText(tc.status, []byte(tc.body))
		if !strings.Contains(got, tc.want) {
			t.Errorf("status %d produced %q, which does not tell the model %q", tc.status, got, tc.want)
		}
	}
}

func TestToolListIsSerialisable(t *testing.T) {
	b, err := json.Marshal(map[string]any{"tools": tools})
	if err != nil {
		t.Fatalf("the tool list does not marshal: %v", err)
	}
	if !strings.Contains(string(b), "inputSchema") {
		t.Error("inputSchema is missing from the wire form; clients key on that exact name")
	}
}
