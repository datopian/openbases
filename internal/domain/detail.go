package domain

import (
	"context"
	"database/sql"
	"time"

	"github.com/datopian/workgraph/internal/authz"
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

	detail := ProjectDetail{ProjectSummary: summary}

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
				rs = &RepositoryStatus{FullName: full}
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
	var signals []Signal

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
