// Package chiefofstaff answers the default questions from plan section 4.4.
//
// Two design decisions shape everything here.
//
// First, the answers are STRUCTURED QUERIES, not a prompt. The plan is explicit
// that this layer "must not receive the whole company as an unbounded prompt",
// and the default questions are all answerable from the database. Running them
// through a model would add cost, latency and a source of invention to
// questions that have exact answers.
//
// Second, every query runs under the signed-in user's identity, so row-level
// security decides what can be read. That is what makes "restricted project
// data never appears for an unauthorised user" a property of the system rather
// than a promise about prompt construction: data the user may not see is never
// fetched, so it cannot leak into an answer or into a model's context later.
package chiefofstaff

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/datopian/openbases/internal/authz"
)

// Kind separates what is known from what is computed from what is guessed.
//
// Collapsing these is how a dashboard becomes untrustworthy: a reader cannot
// challenge "at risk" without knowing whether it was measured, derived from a
// rule, or produced by a model.
type Kind string

const (
	// Observed is a fact read directly from a record.
	Observed Kind = "observed"
	// Calculated is derived from observed facts by a rule stated in Basis.
	Calculated Kind = "calculated"
	// Inferred came from a model. Nothing in this package produces one yet;
	// the value exists so that when it does, it is labelled from the start
	// rather than retrofitted.
	Inferred Kind = "inferred"
)

// Evidence points at the record a claim came from.
type Evidence struct {
	Kind string `json:"kind"` // project, pull_request, approval_request, bead
	Ref  string `json:"ref"`
	Note string `json:"note,omitempty"`
}

// Claim is one sentence with its provenance.
//
// Statement is prose because the answer is read by a person. Everything else
// exists so they can disagree with it: what kind of claim it is, what it came
// from, and how old that is.
type Claim struct {
	Statement  string     `json:"statement"`
	Kind       Kind       `json:"kind"`
	Basis      string     `json:"basis,omitempty"`
	Evidence   []Evidence `json:"evidence"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// Answer is a question and the claims that answer it.
type Answer struct {
	Question string  `json:"question"`
	Claims   []Claim `json:"claims"`
	// Scope states what the answer covered, so an empty answer can be told
	// apart from an answer about nothing.
	Scope string `json:"scope"`
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Questions are the supported questions, in the order the interface offers them.
var Questions = []string{
	"What changed since yesterday?",
	"What needs me today?",
	"Which commitments are at risk?",
	"What is happening across my projects?",
}

// Ask answers one of the supported questions.
func (s *Store) Ask(ctx context.Context, userID, question string) (Answer, error) {
	switch question {
	case Questions[0]:
		return s.whatChanged(ctx, userID, 24*time.Hour)
	case Questions[1]:
		return s.whatNeedsMe(ctx, userID)
	case Questions[2]:
		return s.atRisk(ctx, userID)
	case Questions[3]:
		return s.acrossProjects(ctx, userID)
	default:
		// Refused rather than passed to a model. An unsupported question
		// answered by invention is worse than one answered not at all, and the
		// list of what IS answerable is the useful reply.
		return Answer{}, fmt.Errorf("unsupported question; supported: %v", Questions)
	}
}

func (s *Store) whatChanged(ctx context.Context, userID string, window time.Duration) (Answer, error) {
	a := Answer{Question: Questions[0], Claims: []Claim{},
		Scope: "Pull request activity projected for repositories in projects you can see."}
	since := time.Now().Add(-window)

	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT p.slug, r.owner || '/' || r.name, pr.number, pr.title, pr.state, pr.updated_at
			  FROM pull_request_projections pr
			  JOIN project_repositories r ON r.id = pr.repository_id
			  JOIN projects p ON p.id = r.project_id
			 WHERE pr.updated_at >= $1
			 ORDER BY pr.updated_at DESC
			 LIMIT 20`, since)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var slug, repo, title, state string
			var number int
			var updated time.Time
			if err := rows.Scan(&slug, &repo, &number, &title, &state, &updated); err != nil {
				return err
			}
			t := updated
			a.Claims = append(a.Claims, Claim{
				Statement: fmt.Sprintf("%s #%d (%s) is %s: %s", repo, number, slug, state, title),
				Kind:      Observed,
				Evidence: []Evidence{
					{Kind: "pull_request", Ref: fmt.Sprintf("%s#%d", repo, number)},
					{Kind: "project", Ref: slug},
				},
				ObservedAt: &t,
			})
		}
		return rows.Err()
	})
	if err != nil {
		return Answer{}, err
	}
	if len(a.Claims) == 0 {
		a.Claims = append(a.Claims, Claim{
			Statement: "No pull request activity has been recorded in the last day for projects you can see.",
			Kind:      Observed,
			Basis:     "Absence of projected events. If an integration has stopped delivering this would look the same, so check repository coverage on the project page before concluding nothing happened.",
			Evidence:  []Evidence{},
		})
	}
	return a, nil
}

func (s *Store) whatNeedsMe(ctx context.Context, userID string) (Answer, error) {
	a := Answer{Question: Questions[1], Claims: []Claim{},
		Scope: "Approval requests awaiting a decision, and open attention items assigned to you."}

	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT r.id::text, r.action_type, r.risk_level, COALESCE(p.slug, ''),
			       r.expires_at, r.required_approvals,
			       (SELECT count(*) FROM approval_decisions d
			         WHERE d.request_id = r.id AND d.decision = 'approve')
			  FROM approval_requests r
			  LEFT JOIN projects p ON p.id = r.project_id
			 WHERE r.status = 'pending'
			   AND r.expires_at > now()
			   AND (r.requested_by_user_id IS DISTINCT FROM $1::uuid)
			 ORDER BY r.expires_at
			 LIMIT 20`, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, action, risk, slug string
			var expires time.Time
			var required, recorded int
			if err := rows.Scan(&id, &action, &risk, &slug, &expires, &required, &recorded); err != nil {
				return err
			}
			a.Claims = append(a.Claims, Claim{
				Statement: fmt.Sprintf("%s on %s is waiting for approval (%s risk, %d of %d recorded, expires %s)",
					action, orNone(slug), risk, recorded, required, expires.Format(time.RFC3339)),
				Kind:  Observed,
				Basis: "Requests you did not raise yourself are excluded from your own approval, so these are ones you could act on.",
				Evidence: []Evidence{
					{Kind: "approval_request", Ref: id},
					{Kind: "project", Ref: orNone(slug)},
				},
			})
		}
		return rows.Err()
	})
	if err != nil {
		return Answer{}, err
	}
	if len(a.Claims) == 0 {
		a.Claims = append(a.Claims, Claim{
			Statement: "Nothing is waiting on a decision from you.",
			Kind:      Observed,
			Basis:     "No pending, unexpired approval requests are visible to you that you did not raise yourself.",
			Evidence:  []Evidence{},
		})
	}
	return a, nil
}

func (s *Store) atRisk(ctx context.Context, userID string) (Answer, error) {
	a := Answer{Question: Questions[2], Claims: []Claim{},
		Scope: "Projects you can see, judged on integration coverage and failing checks."}

	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT p.slug,
			       count(DISTINCT r.id),
			       count(DISTINCT r.id) FILTER (
			         WHERE EXISTS (SELECT 1 FROM pull_request_projections x
			                        WHERE x.repository_id = r.id)),
			       count(DISTINCT pr.id) FILTER (
			         WHERE pr.state = 'open' AND pr.checks_state = 'failure')
			  FROM projects p
			  LEFT JOIN project_repositories r ON r.project_id = p.id
			  LEFT JOIN pull_request_projections pr ON pr.repository_id = r.id
			 GROUP BY p.slug
			 ORDER BY p.slug`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var slug string
			var repos, reporting, failing int
			if err := rows.Scan(&slug, &repos, &reporting, &failing); err != nil {
				return err
			}
			switch {
			case repos > 0 && reporting == 0:
				a.Claims = append(a.Claims, Claim{
					Statement: fmt.Sprintf("%s has %d repositories and none has ever reported activity.", slug, repos),
					// Calculated, not observed: the absence is a fact, calling
					// it a risk is a rule.
					Kind:     Calculated,
					Basis:    "A registered repository that has never produced a projected event may mean no work, or an integration that is not delivering. The two are indistinguishable from here, which is itself the risk.",
					Evidence: []Evidence{{Kind: "project", Ref: slug}},
				})
			case failing > 0:
				a.Claims = append(a.Claims, Claim{
					Statement: fmt.Sprintf("%s has %d open pull request(s) with failing checks.", slug, failing),
					Kind:      Calculated,
					Basis:     "Counted from check suites that reported a failed or timed-out conclusion for the commit currently under review.",
					Evidence:  []Evidence{{Kind: "project", Ref: slug}},
				})
			}
		}
		return rows.Err()
	})
	if err != nil {
		return Answer{}, err
	}
	if len(a.Claims) == 0 {
		a.Claims = append(a.Claims, Claim{
			Statement: "No project you can see is showing a risk signal.",
			Kind:      Calculated,
			Basis:     "No visible project has repositories that have never reported, and none has open pull requests with failing checks.",
			Evidence:  []Evidence{},
		})
	}
	return a, nil
}

func (s *Store) acrossProjects(ctx context.Context, userID string) (Answer, error) {
	a := Answer{Question: Questions[3], Claims: []Claim{},
		Scope: "Every project you can see."}

	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT p.slug, p.status, p.visibility,
			       count(DISTINCT r.id),
			       count(DISTINCT pr.id) FILTER (WHERE pr.state = 'open'),
			       max(pr.updated_at)
			  FROM projects p
			  LEFT JOIN project_repositories r ON r.project_id = p.id
			  LEFT JOIN pull_request_projections pr ON pr.repository_id = r.id
			 GROUP BY p.slug, p.status, p.visibility
			 ORDER BY p.slug`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var slug, status, visibility string
			var repos, open int
			var latest sql.NullTime
			if err := rows.Scan(&slug, &status, &visibility, &repos, &open, &latest); err != nil {
				return err
			}
			c := Claim{
				Statement: fmt.Sprintf("%s is %s, %s, with %d repositories and %d open pull requests.",
					slug, status, visibility, repos, open),
				Kind:     Observed,
				Evidence: []Evidence{{Kind: "project", Ref: slug}},
			}
			if latest.Valid {
				t := latest.Time
				c.ObservedAt = &t
			}
			a.Claims = append(a.Claims, c)
		}
		return rows.Err()
	})
	if err != nil {
		return Answer{}, err
	}
	if len(a.Claims) == 0 {
		a.Claims = append(a.Claims, Claim{
			Statement: "You cannot see any projects.",
			Kind:      Observed,
			Basis:     "No project row is visible under your identity. That may mean none exist, or that you are not a member of any.",
			Evidence:  []Evidence{},
		})
	}
	return a, nil
}

func orNone(s string) string {
	if s == "" {
		return "(no project)"
	}
	return s
}
