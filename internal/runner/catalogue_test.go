package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A node that has not been deployed yet still runs. Failing closed on an absent
// catalogue would turn a configuration convenience into a new way for every run
// on the node to stop.
func TestAMissingCatalogueIsNotAnError(t *testing.T) {
	c, err := LoadCatalogue(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("a missing catalogue should be silent: %v", err)
	}
	if c != nil {
		t.Error("a missing catalogue should be nil, so the built-in tables are used")
	}
}

// A file that exists and is wrong is somebody having configured something and
// got it wrong. Running on defaults there is how a deployment believes it
// changed a model and did not.
func TestAMalformedCatalogueIsAnError(t *testing.T) {
	if _, err := LoadCatalogue(write(t, "{not json")); err == nil {
		t.Error("malformed JSON was accepted")
	}
}

// Per key, not per table: naming one role must not blank the others.
func TestACatalogueOverridesOnlyWhatItNames(t *testing.T) {
	p := write(t, `{"models":{"polecat":"workers-ai/@cf/moonshotai/kimi-k2.7-code"}}`)
	c, err := LoadCatalogue(p)
	if err != nil {
		t.Fatal(err)
	}
	s := spec()
	s.Catalogue = c
	s.Runtime = RuntimeOpenCode
	s.GatewayBaseURL = "https://gateway.example/v1/a/g"
	plan, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Model != "workers-ai/@cf/moonshotai/kimi-k2.7-code" {
		t.Errorf("the catalogue's model was ignored: %q", plan.Model)
	}
	// Effort was not named, so it comes from the built-in table.
	if plan.Effort != "medium" {
		t.Errorf("effort = %q, want the built-in medium", plan.Effort)
	}
	// crew was not named either, and must still be a role that exists.
	s2 := spec()
	s2.Catalogue = c
	s2.Role = "crew"
	if _, err := New(s2); err != nil {
		t.Errorf("naming polecat removed crew: %v", err)
	}
}

// The failures a catalogue can cause are the quiet kind, so they are caught at
// load rather than at dispatch on a node.
func TestValidationCatchesWhatWouldFailLater(t *testing.T) {
	for name, body := range map[string]string{
		"a bare model name":      `{"models":{"polecat":"claude-sonnet-5"}}`,
		"a model with no limits": `{"models":{"polecat":"workers-ai/@cf/nobody/unknown"}}`,
		"an unknown runtime":     `{"runtimes":{"polecat":"telepathy"}}`,
		"a non-positive limit":   `{"limits":{"a/b":{"context":0,"output":10}}}`,
	} {
		if _, err := LoadCatalogue(write(t, body)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A catalogue may add a role the binary has never heard of, which is the point:
// a new role should not need a release.
func TestACatalogueCanAddARole(t *testing.T) {
	c, err := LoadCatalogue(write(t, `{
	  "runtimes":{"archivist":"claude"},
	  "models":{"archivist":"anthropic/claude-haiku-4-5"},
	  "effort":{"archivist":"low"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	s := spec()
	s.Catalogue = c
	s.Role = "archivist"
	s.AllowedTools = []string{"Read"} // no built-in allowlist for a new role
	p, err := New(s)
	if err != nil {
		t.Fatalf("a catalogue-defined role was refused: %v", err)
	}
	if p.Model != "anthropic/claude-haiku-4-5" || p.Effort != "low" {
		t.Errorf("model=%q effort=%q", p.Model, p.Effort)
	}
}

// The tool allowlist stays in Go. Whether an agent may run arbitrary shell is a
// change to code somebody reviews, not to a data file on a node.
func TestTheCatalogueCannotWidenTheToolAllowlist(t *testing.T) {
	body := `{"tools":{"polecat":["Bash"]},"models":{"polecat":"anthropic/claude-sonnet-5"}}`
	c, err := LoadCatalogue(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	s := spec()
	s.Catalogue = c
	p, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range p.AllowedTools {
		if tool == "Bash" {
			t.Fatal("a catalogue granted unrestricted Bash; the allowlist must not be configurable")
		}
	}
	if !strings.Contains(strings.Join(p.AllowedTools, " "), "Bash(bd:*)") {
		t.Errorf("the built-in allowlist was not used: %v", p.AllowedTools)
	}
}
