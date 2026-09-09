package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/datopian/openbases/internal/authz"
)

// ProjectDetail is everything the project page shows.
//
// Note what is NOT here: a single overall health value. WP-F1 requires that
// every derived status explains its inputs and freshness, and a lone green dot
// explains nothing — it is the shape that lets a stale or empty input look like
// good news. Each signal carries its own evidence and its own observed time,
// and the page shows them rather than averaging them into a mood.
type ProjectDetail struct {
	ProjectSummary
	Repositories []RepositoryStatus `json:"repositories"`
	Signals      []Signal           `json:"signals"`
	// Work is the project's beads (wg-43n).
	//
	// Absent until now, which is the whole of "I have no visibility of beads in
	// existing projects": the beads were visible on the global Work page all
	// along, and opening a project said nothing about them.
	Work []WorkItem `json:"work"`
}

// WorkItem is one bead as the project page shows it.
//
// A projection of a projection: work_refs caches Beads, and this reads that
// cache. Beads stays canonical, and last_seen says how stale this view is
// rather than pretending it is live.
type WorkItem struct {
	Bead     string     `json:"bead"`
	Title    string     `json:"title"`
	Kind     string     `json:"kind"`
	Status   string     `json:"status"`
	Cell     string     `json:"cell,omitempty"`
	LastSeen *time.Time `json:"last_seen"`
	// BlockedBy is the beads that must finish first, by id.
	//
	// Without this a flat list shows work that cannot start as though nobody
	// had picked it up -- and dependency order is the whole point of `bd
	// ready`. Empty means nothing blocks it, which is the common case and the
	// reason the field is omitted rather than rendered as "none".
	BlockedBy []string `json:"blocked_by,omitempty"`
	// Blocking is the beads waiting on this one. Shown because it is the
	// question a reader asks about a stuck bead: not "what is it waiting for"
	// but "what does finishing it release".
	Blocking []string `json:"blocking,omitempty"`
	// Outcome is what happened to it: never_dispatched, queued, running,
	// failed, landed, blocked or done.
	//
	// Carried per row because a list that cannot say this shows a bead that
	// ran for six minutes and delivered nothing as though nobody had picked it
	// up -- which is what the dependency graph did, in green.
	Outcome string `json:"outcome,omitempty"`
}

// RepositoryStatus is one repository and the pull requests projected for it.
type RepositoryStatus struct {
	FullName     string            `json:"full_name"`
	PullRequests []PullRequestView `json:"pull_requests"`
	// LastProjected is when a delivery for this repository last landed. Nil
	// means nothing has ever been projected, which is different from "quiet"
	// and the page says so.
	LastProjected *time.Time `json:"last_projected"`
}

type PullRequestView struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	State       string     `json:"state"`
	Author      string     `json:"author"`
	ChecksState string     `json:"checks_state"`
	UpdatedAt   time.Time  `json:"updated_at"`
	MergedAt    *time.Time `json:"merged_at"`
}

// Signal is a derived statement with the evidence behind it.
//
// Basis is the sentence a reader needs to disagree with the conclusion, and
// ObservedAt is what tells them whether the conclusion is still worth
// believing. A signal with neither is a rumour.
type Signal struct {
	Name       string     `json:"name"`
	Value      string     `json:"value"`
	Basis      string     `json:"basis"`
	ObservedAt *time.Time `json:"observed_at"`
}

// ProjectDetailBySlug returns the project page, or ErrNotFound when the caller
// may not see it.
func (s *Store) ProjectDetailBySlug(ctx context.Context, userID, slug string) (ProjectDetail, error) {
	summary, err := s.ProjectBySlug(ctx, userID, slug)
	if err != nil {
		return ProjectDetail{}, err
	}

	// Both slices start empty rather than nil, for the same reason: a client
	// that receives null where it expects a list has no good option.
	detail := ProjectDetail{
		ProjectSummary: summary,
		Repositories:   []RepositoryStatus{},
		Signals:        []Signal{},
		Work:           []WorkItem{},
	}

	err = authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		// Repositories and their projections. Row-level security decides which
		// rows come back; this query does no filtering of its own and cannot
		// forget to.
		rows, err := tx.QueryContext(ctx, `
			SELECT r.owner || '/' || r.name,
			       COALESCE(p.number, 0), COALESCE(p.title, ''), COALESCE(p.state, ''),
			       COALESCE(p.author, ''), COALESCE(p.checks_state, ''),
			       p.updated_at, p.merged_at
			  FROM project_repositories r
			  LEFT JOIN pull_request_projections p ON p.repository_id = r.id
			 WHERE r.project_id = $1::uuid
			 ORDER BY r.owner, r.name, p.updated_at DESC NULLS LAST`,
			summary.ID)
		if err != nil {
			return err
		}
		defer rows.Close()

		byRepo := map[string]*RepositoryStatus{}
		var order []string
		for rows.Next() {
			var full string
			var pr PullRequestView
			var updated, merged sql.NullTime
			if err := rows.Scan(&full, &pr.Number, &pr.Title, &pr.State,
				&pr.Author, &pr.ChecksState, &updated, &merged); err != nil {
				return err
			}
			rs, seen := byRepo[full]
			if !seen {
				rs = &RepositoryStatus{FullName: full, PullRequests: []PullRequestView{}}
				byRepo[full] = rs
				order = append(order, full)
			}
			// A repository with no projections joins to a NULL row. That is a
			// repository we know about and have never heard from, which the
			// page must distinguish from one with no open work.
			if pr.Number == 0 {
				continue
			}
			if updated.Valid {
				pr.UpdatedAt = updated.Time
				if rs.LastProjected == nil || updated.Time.After(*rs.LastProjected) {
					t := updated.Time
					rs.LastProjected = &t
				}
			}
			if merged.Valid {
				t := merged.Time
				pr.MergedAt = &t
			}
			rs.PullRequests = append(rs.PullRequests, pr)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, full := range order {
			detail.Repositories = append(detail.Repositories, *byRepo[full])
		}

		// The project's beads.
		//
		// Filtered on project_id and nothing else: row-level security decides
		// which work_refs rows exist for this caller, and adding a visibility
		// clause here would be a second, weaker copy of that rule.
		//
		// Ordered so the page opens on what is moving. Beads with a project
		// come from work_refs.project_id, which is set by system_project_bead
		// from a project:<slug> label, or from the cell when the cell serves
		// exactly one project.
		work, err := tx.QueryContext(ctx, `
			SELECT w.bead_id, COALESCE(w.title,''), COALESCE(w.kind,''),
			       COALESCE(w.status,''), COALESCE(c.slug,''), w.last_seen_at,
			       -- Comma-joined rather than a Postgres array, and that is
			       -- not a style choice. This driver hands an array back as
			       -- its text form -- {a,b} -- so scanning one into a Go
			       -- []string fails at RUN TIME with "unsupported Scan,
			       -- storing driver.Value type string into type *[]string".
			       -- It compiles either way, which is how it reached staging
			       -- and 500'd the project page. string_agg is what
			       -- create.go already uses for the same reason.
			       (SELECT COALESCE(string_agg(f.bead_id, ',' ORDER BY f.bead_id), '')
			          FROM work_links l
			          JOIN work_refs f ON f.id = l.from_work_ref
			         WHERE l.to_work_ref = w.id AND l.relation = 'blocks') AS blocked_by,
			       (SELECT COALESCE(string_agg(t.bead_id, ',' ORDER BY t.bead_id), '')
			          FROM work_links l
			          JOIN work_refs t ON t.id = l.to_work_ref
			         WHERE l.from_work_ref = w.id AND l.relation = 'blocks') AS blocking,
			       -- What happened when it was dispatched, so a list can say
			       -- it. Without this a bead that ran for six minutes and
			       -- delivered nothing rendered exactly like one nobody had
			       -- picked up -- green in the graph, "open" in the table.
			       system_bead_outcome(w.bead_id) AS outcome
			  FROM work_refs w
			  LEFT JOIN execution_cells c ON c.id = w.execution_cell_id
			 WHERE w.project_id = $1::uuid
			 ORDER BY (w.status = 'closed'), w.last_seen_at DESC NULLS LAST, w.bead_id`,
			summary.ID)
		if err != nil {
			return err
		}
		defer work.Close()
		for work.Next() {
			var it WorkItem
			var seen sql.NullTime
			var blockedBy, blocking string
			if err := work.Scan(&it.Bead, &it.Title, &it.Kind, &it.Status, &it.Cell, &seen,
				&blockedBy, &blocking, &it.Outcome); err != nil {
				return err
			}
			it.BlockedBy = splitIDs(blockedBy)
			it.Blocking = splitIDs(blocking)
			if seen.Valid {
				t := seen.Time.UTC()
				it.LastSeen = &t
			}
			detail.Work = append(detail.Work, it)
		}
		if err := work.Err(); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return ProjectDetail{}, err
	}

	detail.Signals = deriveSignals(detail)
	return detail, nil
}

// deriveSignals turns the raw rows into statements a non-technical reader can
// act on, each carrying the evidence it came from.
func deriveSignals(d ProjectDetail) []Signal {
	signals := []Signal{}

	// Integration coverage. A project whose repositories have never reported
	// is not healthy and not unhealthy; it is unobserved, and saying so is more
	// useful than a green tick computed from nothing.
	var reporting, total int
	var newest *time.Time
	for _, r := range d.Repositories {
		total++
		if r.LastProjected != nil {
			reporting++
			if newest == nil || r.LastProjected.After(*newest) {
				newest = r.LastProjected
			}
		}
	}
	switch {
	case total == 0:
		signals = append(signals, Signal{
			Name:  "Repository coverage",
			Value: "No repositories registered",
			Basis: "This project has no repositories linked to it, so nothing about its code can be reported here.",
		})
	case reporting == 0:
		signals = append(signals, Signal{
			Name:  "Repository coverage",
			Value: "Not yet observed",
			Basis: plural(total, "repository is", "repositories are") + " registered, but no pull request activity has ever been received for them. This is not the same as no activity: it may mean the integration is not delivering.",
		})
	default:
		signals = append(signals, Signal{
			Name:       "Repository coverage",
			Value:      itoa(reporting) + " of " + itoa(total) + " reporting",
			Basis:      "Counted from repositories that have had at least one pull request projected.",
			ObservedAt: newest,
		})
	}

	// Open work, and how stale the freshest of it is.
	var open int
	var freshest *time.Time
	var failing int
	for _, r := range d.Repositories {
		for _, pr := range r.PullRequests {
			if pr.State == "open" {
				open++
				if freshest == nil || pr.UpdatedAt.After(*freshest) {
					t := pr.UpdatedAt
					freshest = &t
				}
			}
			if pr.ChecksState == "failure" && pr.State == "open" {
				failing++
			}
		}
	}
	signals = append(signals, Signal{
		Name:       "Open pull requests",
		Value:      itoa(open),
		Basis:      "Counted from projected pull requests in the open state. Updated when GitHub delivers an event, and re-checked by reconciliation every fifteen minutes.",
		ObservedAt: freshest,
	})

	if failing > 0 {
		signals = append(signals, Signal{
			Name:       "Failing checks",
			Value:      itoa(failing) + " open " + plural(failing, "pull request has", "pull requests have") + " failing checks",
			Basis:      "A check suite reported a failed or timed-out conclusion for the commit currently under review.",
			ObservedAt: freshest,
		})
	}

	return signals
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// MarshalJSON guarantees the list fields are arrays, never null.
//
// The guarantee belongs on the type rather than on each construction site. A
// nil slice marshals to JSON null; the browser calls .map on it and throws,
// which unmounts the page and leaves a blank screen with no message. Relying on
// every code path to remember to initialise the slices is exactly the kind of
// discipline that holds until the one path that does not — and the failing path
// here is the EMPTY case, which seeded local data never reaches, so it reaches
// production first.
func (d ProjectDetail) MarshalJSON() ([]byte, error) {
	// A local alias, so the custom marshaller does not recurse into itself.
	type detail ProjectDetail
	out := detail(d)
	if out.Repositories == nil {
		out.Repositories = []RepositoryStatus{}
	}
	if out.Signals == nil {
		out.Signals = []Signal{}
	}
	for i := range out.Repositories {
		if out.Repositories[i].PullRequests == nil {
			out.Repositories[i].PullRequests = []PullRequestView{}
		}
	}
	return json.Marshal(out)
}

// splitIDs turns a comma-joined bead list into a slice, or nil when empty.
//
// nil rather than an empty slice, so the field is omitted from the JSON
// entirely: "blocked_by": [] and no field at all mean the same thing to a
// reader, and the interface treats absence as "nothing blocks this".
func splitIDs(joined string) []string {
	if joined == "" {
		return nil
	}
	return strings.Split(joined, ",")
}
