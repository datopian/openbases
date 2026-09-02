// Package publish turns accepted operational candidates into beads.
//
// The other half of WP-H4. Accepting a durable-memory statement writes a
// knowledge_record in the same transaction as the review, because both are rows
// in the same database. Accepting a task cannot work that way: creating a bead
// means running `bd` against a directory on a host's filesystem, which a
// database function cannot do and an API request should not wait on. So review
// records the intent and this runs on a timer.
//
// Safe to run twice and safe to run after a crash. Before creating anything it
// asks the graph whether a bead for that candidate already exists, by a label
// carrying the candidate's id -- so a run that created a bead and then failed
// before recording it adopts that bead instead of creating a second one. One
// accepted statement, one work item, whatever happened to the process.
package publish

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/datopian/workgraph/internal/beads"
	"github.com/datopian/workgraph/internal/domain"
)

// Beads is the part of the Beads client this needs.
//
// Declared here rather than used through beads.Client, because the publisher
// needs exactly two operations and a fake for two methods is a fake somebody
// will read.
type Beads interface {
	Create(ctx context.Context, db beads.DatabaseRef, issue beads.Issue) (domain.WorkRef, error)
	ByLabel(ctx context.Context, db beads.DatabaseRef, label string) ([]beads.Issue, error)
}

// Pending is one accepted candidate awaiting publication, with the graph the
// routing chose for it.
type Pending struct {
	CandidateID   string
	Type          string
	Statement     string
	Due           time.Time
	Visibility    string
	Confidence    float64
	ProjectSlug   string
	OwnerEmail    string
	ReviewerEmail string
	SourceID      string

	GraphID   string
	GraphName string
	GraphPath string
	GraphHost string

	// Blocked is set by the database when routing could not choose a graph.
	// Such a row is returned deliberately rather than filtered out: a
	// candidate a human accepted that silently never became work is the
	// failure this package exists to remove.
	Blocked string
}

// Result counts one pass, so a timer's log line says what happened.
type Result struct {
	Published int
	Adopted   int
	Blocked   int
	Elsewhere int
	Failed    int
}

// Publisher creates beads for accepted candidates and records what it created.
type Publisher struct {
	DB    *sql.DB
	Beads Beads
	// Node is this host's name as the registry records it. A graph on another
	// host is skipped rather than attempted: the path exists over there, and
	// creating the directory here would produce a second empty graph that
	// looks like the real one.
	Node string
	Log  *slog.Logger
}

// LabelPrefix marks a bead with the candidate that caused it.
//
// The graph is the durable record of what has been published, so this label is
// load-bearing: removing it from a bead would let the publisher create a
// duplicate.
const LabelPrefix = "wg-candidate-"

// Run publishes at most limit candidates.
func (p *Publisher) Run(ctx context.Context, limit int) (Result, error) {
	if p.DB == nil || p.Beads == nil {
		return Result{}, errors.New("a publisher needs a database and a beads client")
	}
	log := p.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}

	pending, err := p.pending(ctx, limit)
	if err != nil {
		return Result{}, err
	}

	var res Result
	for _, c := range pending {
		switch {
		case c.Blocked != "":
			res.Blocked++
			log.Error("an accepted candidate cannot be published",
				"candidate", c.CandidateID, "type", c.Type,
				"project", c.ProjectSlug, "reason", c.Blocked)
			continue
		case p.Node != "" && c.GraphHost != "" && c.GraphHost != p.Node:
			res.Elsewhere++
			log.Info("the graph for this candidate is on another host",
				"candidate", c.CandidateID, "graph", c.GraphName, "host", c.GraphHost)
			continue
		}

		adopted, err := p.publish(ctx, c, log)
		switch {
		case err != nil:
			res.Failed++
			// Logged and carried on rather than returned: one candidate whose
			// graph is wedged must not stop the rest, and the row stays
			// pending so the next pass retries it.
			log.Error("publishing an accepted candidate",
				"candidate", c.CandidateID, "graph", c.GraphName, "error", err)
		case adopted:
			res.Adopted++
		default:
			res.Published++
		}
	}
	return res, nil
}

func (p *Publisher) publish(ctx context.Context, c Pending, log *slog.Logger) (adopted bool, err error) {
	ref := beads.DatabaseRef{ID: c.GraphID, Name: c.GraphName, Path: c.GraphPath}
	label := LabelPrefix + c.CandidateID

	// Ask the graph first. This is the idempotency: a bead already carrying
	// this candidate's label was created by an earlier pass that did not get
	// as far as recording it.
	existing, err := p.Beads.ByLabel(ctx, ref, label)
	if err != nil {
		return false, fmt.Errorf("checking for an existing bead: %w", err)
	}

	var bead beads.Issue
	if len(existing) > 0 {
		bead = existing[0]
		adopted = true
		log.Info("a bead for this candidate already exists; recording it",
			"candidate", c.CandidateID, "bead", bead.Ref.BeadID, "graph", c.GraphName)
	} else {
		issue := c.issue(label)
		created, err := p.Beads.Create(ctx, ref, issue)
		if err != nil {
			return false, err
		}
		issue.Ref = created
		bead = issue
	}

	beadID := bead.Ref.BeadID
	if beadID == "" {
		return adopted, errors.New("the bead has no id")
	}

	var workRef string
	if err := p.DB.QueryRowContext(ctx,
		`SELECT system_record_work_publication($1::uuid, $2::uuid, $3, $4, $5)::text`,
		c.CandidateID, c.GraphID, beadID, bead.Title, bead.Type).Scan(&workRef); err != nil {
		return adopted, fmt.Errorf("recording bead %s: %w", beadID, err)
	}

	log.Info("published an accepted candidate",
		"candidate", c.CandidateID, "bead", beadID, "graph", c.GraphName,
		"work_ref", workRef, "adopted", adopted)
	return adopted, nil
}

// issue shapes the bead.
//
// The statement is both the title and the description: the title is trimmed to
// something readable in a list, and the full text is kept below it so the
// reviewer's exact wording survives -- an accepted statement is a decision
// about words, and truncating it in the only place it is stored would edit it.
func (c Pending) issue(label string) beads.Issue {
	labels := []string{label, "wg-" + c.Type}
	if c.ProjectSlug != "" {
		labels = append(labels, "wg-project-"+c.ProjectSlug)
	}

	var desc strings.Builder
	desc.WriteString(c.Statement)
	desc.WriteString("\n\n---\n")
	fmt.Fprintf(&desc, "Accepted from a %s extracted by Workgraph.\n", c.Type)
	if c.ReviewerEmail != "" {
		fmt.Fprintf(&desc, "Reviewed by: %s\n", c.ReviewerEmail)
	}
	fmt.Fprintf(&desc, "Candidate: %s\n", c.CandidateID)
	fmt.Fprintf(&desc, "Source: %s\n", c.SourceID)
	fmt.Fprintf(&desc, "Extractor confidence: %.2f\n", c.Confidence)
	// The classification travels with the work. A bead in a project graph is
	// already scoped to that project, and saying it out loud is what stops the
	// text being pasted somewhere wider (ADR-0013).
	fmt.Fprintf(&desc, "Classification: %s\n", c.Visibility)

	return beads.Issue{
		Title:       title(c.Statement),
		Description: desc.String(),
		Type:        beadType(c.Type),
		Priority:    priority(c.Type),
		Labels:      labels,
		Assignee:    c.OwnerEmail,
		Due:         c.Due,
		ExternalRef: "wg-candidate-" + c.CandidateID,
	}
}

// beadType maps a candidate type onto one bd accepts.
//
// bd's types are bug|feature|task|epic|chore|decision and a custom type needs
// configuration in every graph. The real type is not thrown away: it is a
// `wg-<type>` label on every bead, so a risk stays findable as a risk.
func beadType(candidateType string) string {
	switch candidateType {
	case "task", "commitment":
		return "task"
	case "risk", "question", "market-signal":
		// Not a task: nobody has agreed to do these. chore is bd's nearest
		// thing to "needs attention, is not a deliverable".
		return "chore"
	default:
		return "task"
	}
}

// priority orders the queue.
//
// A commitment outranks a task because someone said it to a client; a risk
// outranks both because it is the thing that makes the others not happen.
// Questions and signals are real but not urgent.
func priority(candidateType string) int {
	switch candidateType {
	case "risk":
		return 1
	case "commitment":
		return 1
	case "task":
		return 2
	default:
		return 3
	}
}

const titleLimit = 110

// title shortens a statement for a list view without cutting a word in half.
func title(statement string) string {
	t := strings.Join(strings.Fields(statement), " ")
	if t == "" {
		return "(an accepted statement with no text)"
	}
	// A statement that is already one sentence keeps its full stop; a longer
	// one is cut at the first sentence boundary if that fits.
	if i := strings.IndexAny(t, ".!?"); i > 20 && i < titleLimit {
		return strings.TrimSpace(t[:i])
	}
	if len(t) <= titleLimit {
		return t
	}
	cut := t[:titleLimit]
	if i := strings.LastIndex(cut, " "); i > 40 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "..."
}

func (p *Publisher) pending(ctx context.Context, limit int) ([]Pending, error) {
	rows, err := p.DB.QueryContext(ctx, `
		SELECT candidate_id::text, candidate_type, statement, due_date, visibility,
		       confidence, coalesce(project_slug,''), coalesce(owner_email,''),
		       coalesce(reviewer_email,''), source_id::text,
		       coalesce(graph_id::text,''), coalesce(graph_name,''),
		       coalesce(graph_path,''), coalesce(graph_host,''),
		       coalesce(blocked_reason,'')
		  FROM system_pending_work_publications($1)`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing candidates awaiting publication: %w", err)
	}
	defer rows.Close()

	var out []Pending
	for rows.Next() {
		var c Pending
		var due sql.NullTime
		if err := rows.Scan(&c.CandidateID, &c.Type, &c.Statement, &due, &c.Visibility,
			&c.Confidence, &c.ProjectSlug, &c.OwnerEmail, &c.ReviewerEmail, &c.SourceID,
			&c.GraphID, &c.GraphName, &c.GraphPath, &c.GraphHost, &c.Blocked); err != nil {
			return nil, err
		}
		if due.Valid {
			c.Due = due.Time
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type nopWriter struct{}

func (nopWriter) Write(b []byte) (int, error) { return len(b), nil }
