package witness

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Report is what an execution node sends the control plane after a pass.
//
// A whole pass rather than one decision at a time, because the interesting
// number is the shape of the pass: forty observations and one escalation is the
// claim this design makes, and it is only visible if the observations arrive
// with the escalation.
type Report struct {
	Cell string `json:"cell"`
	// Repository is "owner/name", taken from the rig's git URL. The control
	// plane maps it to a project; the execution node does not know or need
	// Workgraph project slugs.
	Repository string     `json:"repository"`
	Decisions  []Decision `json:"decisions"`
}

// Outcome is what the control plane did with a report.
type Outcome struct {
	Recorded  int `json:"recorded"`
	Escalated int `json:"escalated"`
	// Unaddressed counts escalations that reached nobody, because the owning
	// project has no lead, no backup operator, and the organisation has no
	// admin. An escalation with no recipient is a failure that looks exactly
	// like a success unless it is counted separately.
	Unaddressed int `json:"unaddressed"`
	// Notified is the number of inbox items written. It is larger than
	// Escalated when a project has several leads.
	Notified int `json:"notified"`
}

// Score orders the escalation against everything else competing for attention.
//
// Work that could be lost outranks work that was merely never submitted, and
// both outrank a session that has gone quiet — which may well turn out to be
// fine. The numbers are on the same 0..1 scale the other inbox rules use.
// Scored by SITUATION rather than by the raw reason, for the same reason the
// dedupe key is. Gas Town reports "git-dirty" and "not-idle" for one situation
// depending on transient state, and scoring the reason meant the same inbox item
// was refreshed at 0.9 on one pass and 0.75 on the next — an item that moves up
// and down the list while nothing about the problem has changed.
func Score(d Decision) float64 {
	switch d.Situation() {
	case "work-at-risk":
		// Work that exists and is not safely stored outranks everything else
		// here: it is the only case where waiting can lose something.
		return 0.9
	case "never-submitted":
		return 0.75
	case "stalled":
		return 0.5
	default:
		if d.Action == Escalate {
			return 0.75
		}
		return 0.1
	}
}

// Rule is the inbox rule name a decision is filed under. Operators filter and
// unsubscribe by rule, so a stalled session and a dirty sandbox are different
// rules even though both come from here.
func Rule(d Decision) string {
	if d.Action == Ambiguous {
		return "agent_silent"
	}
	return "agent_work_at_risk"
}

// Ingest records a report and raises the escalations in it.
//
// Recording happens for every decision including the observations, and it
// happens before the escalation, so a failure to reach anybody still leaves the
// evidence of what was seen.
func Ingest(ctx context.Context, db *sql.DB, r Report) (Outcome, error) {
	var out Outcome

	owner, name, ok := splitRepository(r.Repository)
	if !ok && len(r.Decisions) > 0 {
		return out, fmt.Errorf("repository %q is not owner/name", r.Repository)
	}

	// Resolved once per report. A rig maps to one repository, so looking it up
	// per decision would be the same query forty times.
	var slug sql.NullString
	if ok {
		if err := db.QueryRowContext(ctx,
			`SELECT system_project_for_repository($1, $2)`, owner, name).Scan(&slug); err != nil {
			return out, fmt.Errorf("resolving the project for %s: %w", r.Repository, err)
		}
	}

	for _, d := range r.Decisions {
		if _, err := db.ExecContext(ctx,
			`SELECT system_record_agent_health($1, $2, $3, $4, $5, $6, $7, $8)`,
			r.Cell, d.Rig, d.Polecat, string(d.Action), d.Reason, d.Basis, d.Bead,
			// False, always, and written down rather than assumed. If a future
			// escalation path does call a model, this is the column that will
			// show it.
			false,
		); err != nil {
			return out, fmt.Errorf("recording %s/%s: %w", d.Rig, d.Polecat, err)
		}
		out.Recorded++

		if d.Action != Escalate && d.Action != Ambiguous {
			continue
		}
		out.Escalated++

		if !slug.Valid || slug.String == "" {
			// Nobody owns this repository in the registry, so there is nobody
			// to tell. Counted, not dropped silently.
			out.Unaddressed++
			continue
		}

		explanation, err := json.Marshal(map[string]any{
			"source":  "witness",
			"cell":    r.Cell,
			"rig":     d.Rig,
			"polecat": d.Polecat,
			"action":  string(d.Action),
			// Both: the situation is what the item is keyed and ranked on, and
			// the raw reason is what Gas Town actually said. Keeping only the
			// normalised form would hide a new reason worth classifying.
			"situation": d.Situation(),
			"reason":    d.Reason,
			"basis":     d.Basis,
			"bead":      d.Bead,
			"silence":   d.Silence,
		})
		if err != nil {
			return out, fmt.Errorf("encoding the explanation: %w", err)
		}

		var notified int
		if err := db.QueryRowContext(ctx,
			`SELECT system_raise_attention($1, $2, $3, $4, $5::jsonb, $6)`,
			slug.String, Rule(d), d.DedupeKey(), Score(d), string(explanation), "internal",
		).Scan(&notified); err != nil {
			return out, fmt.Errorf("raising attention for %s/%s: %w", d.Rig, d.Polecat, err)
		}
		if notified == 0 {
			out.Unaddressed++
		}
		out.Notified += notified
	}
	return out, nil
}

func splitRepository(s string) (owner, name string, ok bool) {
	parts := strings.Split(strings.TrimSpace(s), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
