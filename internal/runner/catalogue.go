package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Catalogue is the model and role configuration a deployment can change without
// a binary release (wg-3tp).
//
// The tables it replaces changed monthly while living as map literals in Go, so
// swapping a role onto a newer model meant building, releasing and deploying a
// binary to alter four lines of data. The data now ships as a file and the
// binary reads it.
//
// What is deliberately NOT here: the tool allowlist. DefaultTools decides
// whether an agent may run arbitrary shell, and moving it into deployed
// configuration would make widening it an edit to a data file rather than a
// change to code somebody reviews. Plan §8.3 makes allowed tools part of the
// signed capability; a config file is the wrong altitude for that.
//
// Every field is optional. A catalogue that sets only `models` leaves runtimes,
// effort and limits at the built-in defaults, so a deployment overrides what it
// means to and inherits the rest.
type Catalogue struct {
	// Runtimes maps a role to the CLI that runs it.
	Runtimes map[string]Runtime `json:"runtimes,omitempty"`
	// Models maps a role to its <provider>/<model>.
	Models map[string]string `json:"models,omitempty"`
	// Effort maps a role to its reasoning effort.
	Effort map[string]string `json:"effort,omitempty"`
	// Limits maps a <provider>/<model> to what it can take.
	Limits map[string]ModelLimits `json:"limits,omitempty"`
}

// LoadCatalogue reads a catalogue, or reports that there is none.
//
// A missing file is not an error and returns nil: a node that has not been
// deployed yet still runs on the built-in tables. Failing closed on an absent
// catalogue would turn a configuration convenience into a new way for every run
// on the node to stop.
//
// A file that exists and is malformed IS an error, because that is somebody
// having tried to configure something and got it wrong — and silently running
// on defaults there is how a deployment believes it changed a model and did not.
func LoadCatalogue(path string) (*Catalogue, error) {
	// An UNSET path is a configuration fault, not an absent file, and the two
	// must not collapse into the same answer.
	//
	// os.IsNotExist is true for "", so an empty path returned (nil, nil) —
	// "no catalogue" — and the caller then used the built-in table. That table
	// says claude and anthropic/claude-sonnet-5, which stopped being what runs
	// when the default became OpenCode on GLM. So a caller that forgot to set
	// the path would not have failed; it would have reported the wrong harness
	// and the wrong model, confidently, on every run. The dispatcher did
	// exactly that for the length of one commit.
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("no model catalogue path was given; an unset path is not the " +
			"same as an absent file, and treating it as one reports the built-in defaults " +
			"as though they were what is running")
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the model catalogue %s: %w", path, err)
	}
	var c Catalogue
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parsing the model catalogue %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Validate refuses a catalogue that would fail later, at dispatch, on a node.
//
// Checked here rather than at first use because the failures are the quiet kind:
// a role whose model has no limits plans fine until something picks that role
// up, and a model missing its provider segment reaches a CLI that answers with a
// warning on stderr and then runs on a default.
func (c *Catalogue) Validate() error {
	if c == nil {
		return nil
	}
	for role, model := range c.Models {
		if _, _, err := splitModel(model); err != nil {
			return fmt.Errorf("role %q: %w", role, err)
		}
		if _, ok := c.limitsFor(model); !ok {
			return fmt.Errorf("role %q maps to %q, which has no limits in this catalogue or the defaults", role, model)
		}
	}
	for _, rt := range c.Runtimes {
		if rt != RuntimeClaude && rt != RuntimeOpenCode {
			return fmt.Errorf("unknown runtime %q", rt)
		}
	}
	for model, l := range c.Limits {
		if l.Context <= 0 || l.Output <= 0 {
			return fmt.Errorf("model %q has a non-positive limit", model)
		}
	}
	return nil
}

// The lookups below take the catalogue where it has an answer and the built-in
// table otherwise, per key rather than per table — so a catalogue that names one
// role does not blank the others.

// PlannedFor reports the runtime and model a role will run on, as strings.
//
// Exported so the dispatcher can tell the control plane what a run is using
// WHILE it runs. Without it the only evidence of which model did the work was
// usage_records, which the cost importer fills hourly — so a bead in progress
// showed no model at all, and the harness was recorded nowhere.
//
// The same resolvers the runner itself uses, deliberately. Reading the
// catalogue again in the dispatcher would be a second implementation of the
// precedence rule (catalogue per key, built-in table otherwise), and the two
// would disagree on the day somebody changed one.
func (c *Catalogue) PlannedFor(role string) (runtime, model string) {
	if rt, ok := c.runtimeFor(role); ok {
		runtime = string(rt)
	}
	if m, ok := c.modelFor(role); ok {
		model = m
	}
	return runtime, model
}

func (c *Catalogue) runtimeFor(role string) (Runtime, bool) {
	if c != nil {
		if rt, ok := c.Runtimes[role]; ok {
			return rt, true
		}
	}
	rt, ok := DefaultRuntimes[role]
	return rt, ok
}

func (c *Catalogue) modelFor(role string) (string, bool) {
	if c != nil {
		if m, ok := c.Models[role]; ok {
			return m, true
		}
	}
	m, ok := DefaultModels[role]
	return m, ok
}

func (c *Catalogue) effortFor(role string) string {
	if c != nil {
		if e, ok := c.Effort[role]; ok {
			return e
		}
	}
	return DefaultEffort[role]
}

func (c *Catalogue) limitsFor(model string) (ModelLimits, bool) {
	if c != nil {
		if l, ok := c.Limits[model]; ok {
			return l, true
		}
	}
	l, ok := GatewayModels[model]
	return l, ok
}

// knownRolesIn lists the roles a catalogue can start, which is the union of what
// it defines and what is built in.
func (c *Catalogue) knownRolesIn() string {
	seen := map[string]bool{}
	for r := range DefaultRuntimes {
		seen[r] = true
	}
	if c != nil {
		for r := range c.Runtimes {
			seen[r] = true
		}
	}
	names := make([]string, 0, len(seen))
	for r := range seen {
		names = append(names, r)
	}
	return joinSorted(names)
}
