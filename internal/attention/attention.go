// Package attention builds the inbox (WP-F2).
//
// The design note on this work package argues that a flat ranked list is "a
// better organised version of overwhelm", and that what helps is movement
// between levels: start high enough to choose a direction, then descend until a
// real next action appears. This package therefore returns a small number of
// candidates per branch rather than every ready item, and treats "this branch
// produced nothing to do" as a result with a reason rather than an empty list.
//
// The reason is derived, not guessed. Beads already distinguishes ready from
// blocked, so "blocked" is a fact; "needs decomposition" and "not ripe" are
// stated only when the data supports them.
package attention

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/datopian/workgraph/internal/authz"
)

// Item is one thing competing for a person's attention.
type Item struct {
	ID          string          `json:"id"`
	Rule        string          `json:"rule"`
	Score       float64         `json:"score"`
	Explanation json.RawMessage `json:"explanation"`
	Status      string          `json:"status"`
	Project     string          `json:"project,omitempty"`
	ProjectSlug string          `json:"project_slug,omitempty"`
	ApprovalID  string          `json:"approval_request_id,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	SnoozedTill *time.Time      `json:"snoozed_until,omitempty"`
}

// Branch is one level of the descent: a project, with a few candidates and, if
// there are none, why.
type Branch struct {
	ProjectSlug string `json:"project_slug"`
	ProjectName string `json:"project_name"`
	Candidates  []Item `json:"candidates"`
	// Outcome is "candidates", "blocked", "nothing_ready" or "none_visible".
	// A branch that yields nothing is a finding, not a gap in the display.
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	// Remaining is how many further candidates exist beyond those shown, so a
	// reader can tell "this is all of it" from "here are the top few".
	Remaining int `json:"remaining"`
}

type Store struct {
	db *sql.DB
	// PerBranch caps how many candidates a branch surfaces. Small on purpose:
	// the point of descending is to avoid being handed everything at once.
	PerBranch int
}

func NewStore(db *sql.DB) *Store { return &Store{db: db, PerBranch: 3} }

// Inbox returns the open items for a user, highest score first.
//
// Snoozed items reappear when their deadline passes rather than being deleted,
// because a snooze is a deferral and losing the item would make snoozing a way
// to silently drop work.
func (s *Store) Inbox(ctx context.Context, userID string) ([]Item, error) {
	items := []Item{}

	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT a.id::text, a.rule, a.score, a.explanation, a.status,
			       COALESCE(p.name, ''), COALESCE(p.slug, ''),
			       COALESCE(a.approval_request_id::text, ''),
			       a.created_at, a.snoozed_until
			  FROM attention_items a
			  LEFT JOIN projects p ON p.id = a.project_id
			 WHERE a.user_id = $1::uuid
			   AND (a.status = 'open'
			        OR (a.status = 'snoozed' AND a.snoozed_until <= now()))
			 ORDER BY a.score DESC, a.created_at`, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it Item
			var snoozed sql.NullTime
			if err := rows.Scan(&it.ID, &it.Rule, &it.Score, &it.Explanation, &it.Status,
				&it.Project, &it.ProjectSlug, &it.ApprovalID, &it.CreatedAt, &snoozed); err != nil {
				return err
			}
			if snoozed.Valid {
				t := snoozed.Time
				it.SnoozedTill = &t
			}
			items = append(items, it)
		}
		return rows.Err()
	})
	return items, err
}

// Descend returns the branches a user can enter, each with a few candidates.
//
// Every visible project appears, including those with nothing to do. A project
// missing from the list would be indistinguishable from a project with no work,
// and the difference matters: one means "nothing needs you", the other means
// "you cannot see this".
func (s *Store) Descend(ctx context.Context, userID string) ([]Branch, error) {
	branches := []Branch{}

	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT p.slug, p.name,
			       COALESCE(a.id::text, ''), COALESCE(a.rule, ''),
			       COALESCE(a.score, 0), COALESCE(a.explanation, '{}'::jsonb),
			       COALESCE(a.status, ''), COALESCE(a.approval_request_id::text, ''),
			       a.created_at
			  FROM projects p
			  LEFT JOIN attention_items a
			    ON a.project_id = p.id
			   AND a.user_id = $1::uuid
			   AND (a.status = 'open'
			        OR (a.status = 'snoozed' AND a.snoozed_until <= now()))
			 ORDER BY p.slug, a.score DESC NULLS LAST`, userID)
		if err != nil {
			return err
		}
		defer rows.Close()

		byProject := map[string]*Branch{}
		var order []string
		for rows.Next() {
			var slug, name, id, rule, status, approval string
			var score float64
			var expl []byte
			var created sql.NullTime
			if err := rows.Scan(&slug, &name, &id, &rule, &score, &expl, &status, &approval, &created); err != nil {
				return err
			}
			b, seen := byProject[slug]
			if !seen {
				b = &Branch{ProjectSlug: slug, ProjectName: name, Candidates: []Item{}}
				byProject[slug] = b
				order = append(order, slug)
			}
			if id == "" {
				continue // the LEFT JOIN's empty row: a project with nothing open
			}
			if len(b.Candidates) >= s.PerBranch {
				b.Remaining++
				continue
			}
			it := Item{ID: id, Rule: rule, Score: score, Explanation: expl,
				Status: status, ProjectSlug: slug, Project: name, ApprovalID: approval}
			if created.Valid {
				it.CreatedAt = created.Time
			}
			b.Candidates = append(b.Candidates, it)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		for _, slug := range order {
			b := byProject[slug]
			b.Outcome, b.Reason = outcomeFor(ctx, tx, b, userID)
			branches = append(branches, *b)
		}
		return nil
	})
	return branches, err
}

// outcomeFor explains an empty branch.
//
// The distinction that matters is between "nothing needs you" and "everything
// here is waiting on something else". A reader shown an empty list assumes the
// first; if the truth is the second, they walk away from a project that is
// stuck.
func outcomeFor(ctx context.Context, tx *sql.Tx, b *Branch, userID string) (string, string) {
	if len(b.Candidates) > 0 {
		return "candidates", ""
	}

	// Is there work here at all that is simply assigned elsewhere or blocked?
	var blocked, total int
	err := tx.QueryRowContext(ctx, `
		SELECT
		  count(*) FILTER (WHERE r.status IN ('pending')),
		  count(*)
		FROM approval_requests r
		JOIN projects p ON p.id = r.project_id
		WHERE p.slug = $1`, b.ProjectSlug).Scan(&blocked, &total)
	if err != nil {
		// Not fatal: an unexplained branch is worse than a slow one, but a
		// failed explanation must not take the whole inbox down.
		return "nothing_ready", "Could not determine why this project has nothing for you."
	}

	switch {
	case blocked > 0:
		return "blocked", fmt.Sprintf(
			"%d approval request(s) here are pending someone else's decision, so nothing is waiting on you.", blocked)
	case total > 0:
		return "nothing_ready", "Everything raised here has already been decided."
	default:
		return "nothing_ready", "No approvals or flagged work exist for this project yet."
	}
}

// Snooze defers an item.
func (s *Store) Snooze(ctx context.Context, userID, itemID string, until time.Time) error {
	if !until.After(time.Now()) {
		return errors.New("a snooze must end in the future")
	}
	return s.update(ctx, userID, itemID,
		`UPDATE attention_items SET status = 'snoozed', snoozed_until = $3
		  WHERE id = $2::uuid AND user_id = $1::uuid AND status IN ('open','snoozed')`,
		until)
}

// Delegate hands an item to someone else.
//
// The item moves rather than being copied: two people each believing the other
// is handling it is the failure delegation is supposed to remove.
func (s *Store) Delegate(ctx context.Context, userID, itemID, toUserID string) error {
	if toUserID == "" {
		return errors.New("delegation needs a recipient")
	}
	if toUserID == userID {
		return errors.New("delegating to yourself changes nothing")
	}
	return s.update(ctx, userID, itemID,
		`UPDATE attention_items
		    SET status = 'delegated', delegated_to_user_id = $3::uuid, user_id = $3::uuid
		  WHERE id = $2::uuid AND user_id = $1::uuid AND status IN ('open','snoozed')`,
		toUserID)
}

// Resolve closes an item.
func (s *Store) Resolve(ctx context.Context, userID, itemID string) error {
	return s.update(ctx, userID, itemID,
		`UPDATE attention_items SET status = 'resolved', resolved_at = now()
		  WHERE id = $2::uuid AND user_id = $1::uuid AND status <> 'resolved'`)
}

func (s *Store) update(ctx context.Context, userID, itemID, query string, args ...any) error {
	if itemID == "" {
		return errors.New("no item")
	}
	return authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		all := append([]any{userID, itemID}, args...)
		res, err := tx.ExecContext(ctx, query, all...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// The item is someone else's, already resolved, or does not exist.
			// One answer for all three: which it is would tell a caller about
			// items they cannot see.
			return errors.New("no such open item")
		}
		return nil
	})
}
