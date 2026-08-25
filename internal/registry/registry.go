// Package registry describes which execution nodes and cells exist, and which
// project's work runs in each.
//
// These are facts Ansible already knows — the inventory names the nodes, and
// group_vars/execution.yml declares the cells — and that nothing ever wrote
// down. The result was that execution_nodes and execution_cells were both empty
// on staging while a cell was running and had produced 9,200 agent health
// events (wg-38w).
//
// The document is validated as a pure function, separated from the writing, for
// the same reason the witness, the monitor and the cost mapping are: the
// interesting cases are a typo in a cell name and a project pointing at a cell
// that does not exist, and both are trivial as table-driven tests and awkward to
// arrange against a live database.
package registry

import (
	"fmt"
	"sort"
	"strings"
)

// Document is the desired state of one node and its cells.
type Document struct {
	Node     Node      `json:"node"`
	Cells    []Cell    `json:"cells"`
	Projects []Project `json:"projects"`
}

// Node is one execution host.
type Node struct {
	Hostname    string `json:"hostname"`
	Environment string `json:"environment"`
}

// Cell is one trust domain on a node, as APPLIED rather than as declared.
//
// The limits matter: the execution_cell role derives them from the node's real
// CPU and memory because the declared numbers were above the hardware and could
// never be reached. Recording the declared values would put that fiction into
// the database.
type Cell struct {
	Slug                string `json:"slug"`
	SystemUsername      string `json:"system_username"`
	TrustDomain         string `json:"trust_domain"`
	MaxConcurrentAgents int    `json:"max_concurrent_agents"`
	CPUQuotaPercent     int    `json:"cpu_quota_percent"`
	MemoryLimitMB       int    `json:"memory_limit_mb"`
}

// Project attaches a project to the cell its work runs in.
type Project struct {
	Slug string `json:"slug"`
	Cell string `json:"cell"`
	// Visibility tightens the project now that it has a cell, and is empty for
	// every project that does not need it. It exists for the case 0009 wrote
	// down: nged is a restricted engagement created as confidential because the
	// schema refuses a restricted project with no execution_cell_id.
	Visibility string `json:"visibility,omitempty"`
}

var (
	validEnvironments = map[string]bool{"staging": true, "production": true}
	validVisibility   = map[string]bool{"internal": true, "confidential": true, "restricted": true}
)

// Validate reports everything wrong with a document, not just the first thing.
//
// All of them, because these are usually typed by hand into group_vars and
// fixing them one deployment at a time is how a five-minute edit becomes an
// afternoon.
func (d Document) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if strings.TrimSpace(d.Node.Hostname) == "" {
		add("the node has no hostname")
	}
	if !validEnvironments[d.Node.Environment] {
		add("node %q has environment %q, which is not one of %s",
			d.Node.Hostname, d.Node.Environment, keys(validEnvironments))
	}
	if len(d.Cells) == 0 {
		add("node %q declares no cells", d.Node.Hostname)
	}

	seen := map[string]bool{}
	for i, c := range d.Cells {
		where := fmt.Sprintf("cell %d", i)
		if s := strings.TrimSpace(c.Slug); s == "" {
			add("%s has no slug", where)
		} else {
			where = fmt.Sprintf("cell %q", s)
			if seen[s] {
				// Two cells with one slug would upsert over each other and the
				// second would silently win.
				add("%s is declared twice", where)
			}
			seen[s] = true
		}
		if strings.TrimSpace(c.SystemUsername) == "" {
			add("%s has no system username", where)
		}
		if strings.TrimSpace(c.TrustDomain) == "" {
			// A cell with no trust domain is a cell whose isolation nobody can
			// reason about, which is the whole reason cells exist (ADR-0002).
			add("%s has no trust domain", where)
		}
		if c.MaxConcurrentAgents < 0 {
			add("%s has a negative concurrent-agent limit (%d)", where, c.MaxConcurrentAgents)
		}
		if c.CPUQuotaPercent < 0 {
			add("%s has a negative CPU quota (%d)", where, c.CPUQuotaPercent)
		}
		if c.MemoryLimitMB < 0 {
			add("%s has a negative memory limit (%d)", where, c.MemoryLimitMB)
		}
	}

	for i, p := range d.Projects {
		where := fmt.Sprintf("project %d", i)
		if s := strings.TrimSpace(p.Slug); s == "" {
			add("%s has no slug", where)
		} else {
			where = fmt.Sprintf("project %q", s)
		}
		switch {
		case strings.TrimSpace(p.Cell) == "":
			add("%s names no cell", where)
		case !seen[p.Cell]:
			// The typo that would otherwise reach the database as "no execution
			// cell named client_nged" halfway through a deployment.
			add("%s names cell %q, which this node does not declare (it has %s)",
				where, p.Cell, keys(seen))
		}
		if p.Visibility != "" && !validVisibility[p.Visibility] {
			add("%s asks for visibility %q, which is not one of %s",
				where, p.Visibility, keys(validVisibility))
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("the registry document is not usable:\n  %s", strings.Join(problems, "\n  "))
}

// SharedCells reports cells hosting more than one project.
//
// Not an error — docs/pilot/registry.md explicitly allows portaljs-oss to "share
// an execution cell with other non-sensitive work". But a shared cell cannot
// attribute spend: system_record_usage resolves a cell tag to a project only
// when the cell maps to exactly one, and refuses to guess otherwise. Reporting
// it is what stops that being discovered as a puzzling gap in a cost report.
func (d Document) SharedCells() map[string][]string {
	byCell := map[string][]string{}
	for _, p := range d.Projects {
		byCell[p.Cell] = append(byCell[p.Cell], p.Slug)
	}
	shared := map[string][]string{}
	for cell, projects := range byCell {
		if len(projects) > 1 {
			sort.Strings(projects)
			shared[cell] = projects
		}
	}
	return shared
}

func keys(m map[string]bool) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
