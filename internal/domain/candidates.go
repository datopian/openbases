package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/datopian/openbases/internal/authz"
)

// SourceSpan is one cited excerpt.
type SourceSpan struct {
	Line  int    `json:"line"`
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
	Text  string `json:"text"`
}

// CandidateCard is everything plan section 4.6 requires on a review card.
//
// Assembled in one query rather than left to the client to stitch together: a
// reviewer deciding without the excerpt in front of them is reviewing the
// model's word for it, which is the failure this whole workflow exists to
// prevent.
type CandidateCard struct {
	ID           string       `json:"id"`
	Type         string       `json:"type"`
	Statement    string       `json:"statement"`
	Confidence   float64      `json:"confidence"`
	Inferred     bool         `json:"inferred"`
	Visibility   string       `json:"visibility"`
	DueDate      *string      `json:"due_date"`
	Project      string       `json:"project"`
	Spans        []SourceSpan `json:"source_spans"`
	SourceKind   string       `json:"source_type"`
	Provider     string       `json:"provider"`
	CapturedAt   *time.Time   `json:"captured_at"`
	Participants []string     `json:"participants"`
	// InjectionSuspected is surfaced on the card, not hidden in a log. The
	// source contained instruction-shaped text, so this candidate needs two
	// reviewers, and the person looking at it is the one who has to know.
	InjectionSuspected bool      `json:"injection_suspected"`
	PromptVersion      string    `json:"extractor_prompt_version"`
	CreatedAt          time.Time `json:"created_at"`
}

// PendingCandidates returns the review queue the caller may see.
//
// Row-level security decides which rows come back, through the same
// can_read_source predicate as everything else that touches a source.
func (s *Store) PendingCandidates(ctx context.Context, userID string, limit int) ([]CandidateCard, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := []CandidateCard{}
	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT c.id::text, c.candidate_type, c.statement, c.confidence,
			       c.was_inferred, c.visibility, c.due_date::text,
			       COALESCE(p.slug, ''), c.source_spans,
			       ks.source_type, ks.provider, ks.captured_at,
			       ks.prompt_injection_suspected, COALESCE(c.extractor_prompt_version, ''),
			       c.created_at,
			       -- JSON rather than text[]: arrays cross the database/sql
			       -- boundary differently between drivers, and that cost a
			       -- whole dependency to discover once already.
			       COALESCE((SELECT jsonb_agg(a.principal ORDER BY a.principal)
			                   FROM source_acl_entries a
			                  WHERE a.source_id = ks.id), '[]'::jsonb)
			  FROM knowledge_candidates c
			  JOIN knowledge_sources ks ON ks.id = c.source_id
			  LEFT JOIN projects p ON p.id = c.proposed_project_id
			 WHERE c.status = 'pending'
			 ORDER BY c.created_at, c.confidence DESC
			 LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c CandidateCard
			var spans []byte
			var due sql.NullString
			var captured sql.NullTime
			var participants []byte
			if err := rows.Scan(&c.ID, &c.Type, &c.Statement, &c.Confidence,
				&c.Inferred, &c.Visibility, &due, &c.Project, &spans,
				&c.SourceKind, &c.Provider, &captured,
				&c.InjectionSuspected, &c.PromptVersion, &c.CreatedAt,
				&participants); err != nil {
				return err
			}
			if due.Valid {
				c.DueDate = &due.String
			}
			if captured.Valid {
				t := captured.Time
				c.CapturedAt = &t
			}
			if len(spans) > 0 {
				_ = json.Unmarshal(spans, &c.Spans)
			}
			if len(participants) > 0 {
				_ = json.Unmarshal(participants, &c.Participants)
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// DecidedCard is what happened to a candidate after review.
//
// The queue answered "what needs deciding". This answers the question a
// reviewer asks next and had no way to ask: what did my decision DO. An
// accepted task that produced no bead is visible here as accepted with no
// bead, which is the state worth seeing.
type DecidedCard struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Statement string    `json:"statement"`
	Status    string    `json:"status"`
	Project   string    `json:"project"`
	Reviewer  string    `json:"reviewer"`
	Reason    string    `json:"reason"`
	DecidedAt time.Time `json:"decided_at"`

	// Published names the destination, in the same words review_candidate
	// uses: knowledge_record, bead, queued_beads or none.
	Published string `json:"published"`
	RecordID  string `json:"record_id,omitempty"`
	BeadID    string `json:"bead_id,omitempty"`
	Graph     string `json:"graph,omitempty"`
}

// DecidedCandidates returns recently reviewed candidates and what became of
// them.
//
// Under the same row-level security as the queue, so a reviewer sees the
// history of the material they may read and no more. The bead is joined
// through the candidate's own work reference rather than looked up by label in
// the graph: the control plane's answer to "what did this become" must not
// depend on a graph being reachable.
func (s *Store) DecidedCandidates(ctx context.Context, userID string, limit int) ([]DecidedCard, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := []DecidedCard{}
	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT c.id::text, c.candidate_type, c.statement, c.status,
			       COALESCE(p.slug, ''), COALESCE(ru.primary_email, ''),
			       COALESCE(r.reason, ''), r.decided_at,
			       COALESCE(krs.record_id::text, ''),
			       COALESCE(w.bead_id, ''), COALESCE(b.name, '')
			  FROM knowledge_candidates c
			  JOIN knowledge_sources ks ON ks.id = c.source_id
			  LEFT JOIN projects p ON p.id = c.proposed_project_id
			  -- The deciding review. A candidate can carry several rows once
			  -- edits and merges are in play, and the last one is the one that
			  -- explains the current status.
			  LEFT JOIN LATERAL (
			      SELECT kr.reason, kr.decided_at, kr.reviewer_user_id
			        FROM knowledge_reviews kr
			       WHERE kr.candidate_id = c.id
			       ORDER BY kr.decided_at DESC
			       LIMIT 1
			  ) r ON true
			  LEFT JOIN users ru ON ru.id = r.reviewer_user_id
			  LEFT JOIN knowledge_record_sources krs ON krs.candidate_id = c.id
			  LEFT JOIN work_refs w ON w.id = c.work_ref_id
			  LEFT JOIN beads_databases b ON b.id = w.beads_database_id
			 WHERE c.status <> 'pending'
			 ORDER BY r.decided_at DESC NULLS LAST, c.created_at DESC
			 LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d DecidedCard
			var decided sql.NullTime
			if err := rows.Scan(&d.ID, &d.Type, &d.Statement, &d.Status,
				&d.Project, &d.Reviewer, &d.Reason, &decided,
				&d.RecordID, &d.BeadID, &d.Graph); err != nil {
				return err
			}
			if decided.Valid {
				d.DecidedAt = decided.Time
			}
			d.Published = destination(d)
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// destination says where an accepted statement went, and admits when it has
// not gone anywhere yet.
func destination(d DecidedCard) string {
	switch {
	case d.RecordID != "":
		return "knowledge_record"
	case d.BeadID != "":
		return "bead"
	case d.Status == "accepted" || d.Status == "edited_accepted":
		// Accepted and not published. For an operational type that is the
		// publisher's queue, and for a durable one it should be impossible --
		// either way the reviewer sees it rather than assuming it landed.
		return "queued_beads"
	default:
		return "none"
	}
}

// ErrNotPending is returned when a candidate has already been decided.
var ErrNotPending = errors.New("candidate is not pending")

// ReviewResult is what a decision did.
//
// Published names the destination rather than a boolean, because "accepted"
// and "became durable memory" are different facts and conflating them is how a
// reviewer comes to believe a task is tracked when it is not.
type ReviewResult struct {
	Status    string `json:"status"`
	Published string `json:"published"`
	RecordID  string `json:"record_id,omitempty"`
}

// Review records a decision, moves the candidate, and publishes it.
//
// The two happen in one statement inside the database, because a review
// recorded without the status moving leaves the candidate in the queue to be
// reviewed twice, and a status moved without a review recorded loses the reason.
func (s *Store) Review(ctx context.Context, userID, candidateID, decision, reason, edited string) (ReviewResult, error) {
	var zero ReviewResult
	decision = strings.TrimSpace(decision)
	// Checked here as well as by the schema so the caller gets a 400 that says
	// what is missing, rather than a constraint name.
	switch decision {
	case "reject":
		if strings.TrimSpace(reason) == "" {
			return zero, fmt.Errorf("a rejection needs a reason: it is the evaluation data the improvement loop reads")
		}
	case "edit_and_accept":
		if strings.TrimSpace(edited) == "" {
			return zero, fmt.Errorf("an edit needs the corrected statement, or the correction is not recoverable")
		}
	case "accept", "defer":
	case "merge":
		return zero, fmt.Errorf("merge needs a target record; merging into an existing one lands with the rest of WP-H4")
	default:
		return zero, fmt.Errorf("unknown decision %q", decision)
	}

	var raw []byte
	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT review_candidate($1::uuid, $2, $3, $4)`,
			candidateID, decision, nullIfEmpty(reason), nullIfEmpty(edited)).Scan(&raw)
	})
	if err != nil {
		if strings.Contains(err.Error(), "is not pending") {
			return zero, ErrNotPending
		}
		// A candidate the caller cannot see is indistinguishable from one that
		// does not exist, which is the answer a restricted project needs.
		if strings.Contains(err.Error(), "no rows") {
			return zero, ErrNotFound
		}
		return zero, err
	}
	var out ReviewResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, fmt.Errorf("reading the review result: %w", err)
	}
	return out, nil
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
