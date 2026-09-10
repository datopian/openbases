package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/datopian/openbases/internal/beads"
	"github.com/datopian/openbases/internal/work"
)

// fileplan writes a plan made elsewhere into the rig's graph.
//
// No agent, no model, no money. Planning has moved out of the platform: the
// decision arrives already made and this records it. That is the whole
// difference from a plan job, which starts an agent to decide.
//
// Two passes, and the order is forced. Beads must all exist before any
// dependency can point at one, because a plan routinely puts a bead before
// the thing it depends on -- "deploy" is written above "scaffold" as often as
// below it.
func (d *dispatcher) fileplan(ctx context.Context, job work.Job) (string, error) {
	plan, err := work.DecodePlan(job.Brief)
	if err != nil {
		return "", err
	}
	if err := plan.Validate(); err != nil {
		// Refused before anything is written. A half-filed graph is worse
		// than a refused one: the next re-plan has to reconcile it, and
		// nobody can tell which half landed.
		return "", err
	}

	rig := d.rigFor(job)
	dir := d.cellRoot + "/town/" + rig
	bd := d.bdBinary
	if strings.TrimSpace(bd) == "" {
		bd = "bd"
	}
	client := &beads.CLIClient{
		Binary:        bd,
		Home:          d.cellRoot,
		DatabasePaths: map[string]string{rig: dir},
	}
	db := beads.DatabaseRef{ID: rig, CellID: d.cell, Path: dir}

	// The label that puts a bead on its project's page. Without it the bead
	// exists in the graph and is invisible everywhere a person looks.
	projectLabel := "wg-project-" + plan.Project

	filed := make(map[string]string, len(plan.Beads))
	out := make([]work.Filed, 0, len(plan.Beads))

	for _, b := range plan.Beads {
		key := b.ExternalKey(plan.Project)
		issue := beads.Issue{
			Title:       strings.TrimSpace(b.Title),
			Description: b.Description,
			Acceptance:  b.Acceptance,
			Design:      b.Design,
			Type:        b.Type,
			Priority:    b.Priority,
			Status:      b.Status,
			Labels:      append(append([]string{}, b.Labels...), projectLabel),
			ExternalRef: key,
		}

		existing, found, err := client.ByExternalRef(ctx, db, key)
		if err != nil {
			return "", fmt.Errorf("looking for %s: %w", key, err)
		}
		if found {
			if err := client.UpdateIn(ctx, db, existing.Ref.BeadID, issue); err != nil {
				return "", fmt.Errorf("revising %s (%s): %w", b.Ref, existing.Ref.BeadID, err)
			}
			filed[b.Ref] = existing.Ref.BeadID
			out = append(out, work.Filed{Ref: b.Ref, Bead: existing.Ref.BeadID})
			continue
		}
		ref, err := client.Create(ctx, db, issue)
		if err != nil {
			return "", fmt.Errorf("filing %s: %w", b.Ref, err)
		}
		filed[b.Ref] = ref.BeadID
		out = append(out, work.Filed{Ref: b.Ref, Bead: ref.BeadID, Created: true})
	}

	// Pass two: the edges.
	for _, b := range plan.Beads {
		blocked := filed[b.Ref]
		for _, dep := range b.DependsOn {
			dep = strings.TrimSpace(dep)
			// A ref in this plan, or an id filed long ago. Validate has
			// already refused anything that is neither.
			blocker, ok := filed[dep]
			if !ok {
				blocker = dep
			}
			if err := client.DependOn(ctx, db, blocked, blocker); err != nil {
				// Reported, not fatal. bd refuses an edge it already has, and
				// re-filing an unchanged plan should be a no-op rather than a
				// failure -- that is what "re-plan often" means.
				d.log.Warn("dependency not added", "blocked", blocked,
					"blocker", blocker, "error", err)
			}
		}
	}

	// The other half of --no-cycle-check. A cycle blocks every bead in it for
	// ever, by each other, and nothing in the interface says why -- so it is
	// worth one pass and worth saying loudly.
	if cycles, err := client.Cycles(ctx, db); err != nil {
		d.log.Warn("could not check for dependency cycles", "rig", rig, "error", err)
	} else if cycles != "" {
		d.log.Error("this plan left a dependency cycle", "rig", rig, "cycles", cycles)
		body, _ := json.Marshal(map[string]any{"filed": out, "cycles": cycles})
		return string(body), fmt.Errorf("the beads were filed, but they contain a "+
			"dependency cycle and every bead in it is blocked by the others: %s", cycles)
	}

	body, err := json.Marshal(map[string]any{"filed": out, "project": plan.Project, "rig": rig})
	if err != nil {
		return "", err
	}
	return string(body), nil
}
