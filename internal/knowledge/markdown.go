// Package knowledge renders an accepted durable record as the Markdown file
// company-workgraph keeps under knowledge/.
//
// The record already exists in the database. This is the same fact in Git,
// where a person can read it in a diff and argue with it before it becomes
// company memory (plan section 14.5). knowledge/RECORD-SCHEMA.md is the shape,
// and the reason it exists is stated there: so "Git and PostgreSQL never
// disagree about provenance, classification, or freshness".
//
// Nothing here carries source TEXT. Provenance is a provider, a revision and a
// line reference, which is what the schema asks for -- excerpt_ref "points at
// the evidence span, not the raw text" -- and is also how WP-H4's criterion
// that the raw transcript is not committed stays true by construction rather
// than by review.
package knowledge

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Source is one piece of provenance, by reference.
type Source struct {
	SourceID         string   `json:"source_id"`
	Provider         string   `json:"provider"`
	ProviderRevision string   `json:"provider_revision"`
	ExcerptRefs      []string `json:"excerpt_refs"`
}

// Record is an accepted durable record, as the front matter describes it.
type Record struct {
	ID            string
	Type          string
	Statement     string
	Scope         string
	Project       string
	Function      string
	Visibility    string
	Confidence    float64
	ValidFrom     time.Time
	ReviewAfter   time.Time
	ReviewedAt    time.Time
	Reviewer      string
	Owner         string
	Author        string
	HumanAuthored bool
	Supersedes    string
	Sources       []Source
	// RelatedWork is the bead this record produced, if one exists yet, as the
	// identity tuple the schema asks for -- a bead id alone is ambiguous
	// between graphs (plan section 7.3).
	RelatedWork []WorkRef
}

// WorkRef is the identity tuple of a bead.
type WorkRef struct {
	OrganisationID  string
	ExecutionCellID string
	BeadsDatabaseID string
	BeadID          string
}

// Validate refuses a record the schema would reject, before anything is
// written.
//
// Rule 1 of RECORD-SCHEMA.md: either sources is non-empty, or human_authored
// with a named author. A record with neither is unattributable, and an
// unattributable claim in company memory is worse than a missing one because
// it reads as established.
func (r Record) Validate() error {
	switch {
	case r.ID == "":
		return errors.New("a record needs an id")
	case strings.TrimSpace(r.Statement) == "":
		return errors.New("a record needs a statement")
	case r.Type == "":
		return errors.New("a record needs a type")
	case r.Reviewer == "":
		return errors.New("a record needs the reviewer who accepted it")
	case len(r.Sources) == 0 && !(r.HumanAuthored && r.Author != ""):
		return errors.New("a record needs either a source or a named author")
	case r.Scope == "project" && r.Project == "":
		return errors.New("a project-scoped record must name its project")
	case r.Scope == "function" && r.Function == "":
		return errors.New("a function-scoped record must name its function")
	}
	return nil
}

// Path is where the record lives in the repository.
//
// Grouped the way the directory already is: company-wide records under
// knowledge/company, a project's under knowledge/projects/<slug>. The file name
// carries the acceptance date, a readable fragment of the statement and the
// first bytes of the id -- the date so the directory sorts chronologically, the
// fragment so a reviewer can find it without opening files, and the id so two
// similar statements cannot collide.
func (r Record) Path() string {
	dir := "knowledge/company"
	switch r.Scope {
	case "project":
		dir = "knowledge/projects/" + r.Project
	case "function":
		dir = "knowledge/functions/" + r.Function
	}
	date := r.ReviewedAt
	if date.IsZero() {
		date = time.Now().UTC()
	}
	return fmt.Sprintf("%s/%ss/%s-%s-%s.md",
		dir, r.Type, date.UTC().Format("2006-01-02"), slug(r.Statement), shortID(r.ID))
}

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	out := notSlug.ReplaceAllString(strings.ToLower(s), "-")
	out = strings.Trim(out, "-")
	if len(out) > 48 {
		out = out[:48]
		if i := strings.LastIndex(out, "-"); i > 20 {
			out = out[:i]
		}
	}
	if out == "" {
		out = "record"
	}
	return out
}

func shortID(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Render writes the Markdown file: front matter mirroring knowledge_records,
// then the body a human reads.
func (r Record) Render() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: mem-%s\n", r.ID)
	fmt.Fprintf(&b, "type: %s\n", r.Type)
	// Block scalar, because a statement may contain a colon and a plain scalar
	// with one is not the string somebody meant.
	fmt.Fprintf(&b, "statement: >-\n  %s\n", yamlFold(r.Statement))
	fmt.Fprintf(&b, "scope: %s\n", r.Scope)
	if r.Project != "" {
		fmt.Fprintf(&b, "project: %s\n", r.Project)
	}
	if r.Function != "" {
		fmt.Fprintf(&b, "function: %s\n", r.Function)
	}

	if len(r.Sources) == 0 {
		b.WriteString("sources: []\n")
	} else {
		b.WriteString("sources:\n")
		for _, s := range r.Sources {
			fmt.Fprintf(&b, "  - source_id: %s\n", yamlString(s.SourceID))
			fmt.Fprintf(&b, "    provider: %s\n", s.Provider)
			if s.ProviderRevision != "" {
				fmt.Fprintf(&b, "    provider_revision: %s\n", yamlString(s.ProviderRevision))
			}
			if len(s.ExcerptRefs) > 0 {
				fmt.Fprintf(&b, "    excerpt_refs: [%s]\n", strings.Join(s.ExcerptRefs, ", "))
			}
		}
	}

	fmt.Fprintf(&b, "human_authored: %t\n", r.HumanAuthored)
	if r.Author != "" {
		fmt.Fprintf(&b, "author: %s\n", r.Author)
	}
	if r.Owner != "" {
		fmt.Fprintf(&b, "owner: %s\n", r.Owner)
	}
	fmt.Fprintf(&b, "reviewer: %s\n", r.Reviewer)
	fmt.Fprintf(&b, "reviewed_at: %s\n", dateOf(r.ReviewedAt))
	fmt.Fprintf(&b, "visibility: %s\n", r.Visibility)
	fmt.Fprintf(&b, "confidence: %.2f\n", r.Confidence)
	fmt.Fprintf(&b, "valid_from: %s\n", dateOf(r.ValidFrom))
	if !r.ReviewAfter.IsZero() {
		fmt.Fprintf(&b, "review_after: %s\n", dateOf(r.ReviewAfter))
	}
	if r.Supersedes != "" {
		fmt.Fprintf(&b, "supersedes: mem-%s\n", r.Supersedes)
	}
	b.WriteString("status: accepted\n")

	b.WriteString(renderRelatedWork(r.RelatedWork))
	b.WriteString("---\n\n")

	// The body is deliberately unfinished. This file arrives as a pull request
	// for a person to complete: the statement and its provenance are machine
	// facts and are filled in, the reasoning is not, and a generated paragraph
	// pretending to be reasoning is worse than an empty section that asks for
	// it.
	b.WriteString("## Context\n\n")
	fmt.Fprintf(&b, "Accepted by %s on %s. Add the reasoning that made this worth keeping: what "+
		"question it answers, and what would change if it turned out to be wrong.\n\n",
		r.Reviewer, dateOf(r.ReviewedAt))

	b.WriteString("## Statement\n\n")
	fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(r.Statement))

	b.WriteString("## Evidence\n\n")
	if len(r.Sources) == 0 {
		fmt.Fprintf(&b, "Written by %s. No source: this is a first-hand statement.\n\n", r.Author)
	} else {
		b.WriteString("The sources are referenced rather than copied, so this file carries no " +
			"transcript text (ADR-0027).\n\n")
		for _, s := range r.Sources {
			line := fmt.Sprintf("- `%s` from %s", s.SourceID, s.Provider)
			if s.ProviderRevision != "" {
				line += fmt.Sprintf(", revision `%s`", s.ProviderRevision)
			}
			if len(s.ExcerptRefs) > 0 {
				line += fmt.Sprintf(", at %s", strings.Join(s.ExcerptRefs, ", "))
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}
	for _, w := range r.RelatedWork {
		fmt.Fprintf(&b, "- bead `%s`\n", w.BeadID)
	}
	if len(r.RelatedWork) > 0 {
		b.WriteString("\n")
	}

	b.WriteString("## Consequences\n\n")
	b.WriteString("What changes because this is true. What becomes blocked or unblocked.\n")

	return []byte(b.String()), nil
}

func dateOf(t time.Time) string {
	if t.IsZero() {
		return time.Now().UTC().Format("2006-01-02")
	}
	return t.UTC().Format("2006-01-02")
}

// yamlFold puts a statement in a folded block scalar, indented and wrapped so
// the file reads in a diff.
func yamlFold(s string) string {
	words := strings.Fields(s)
	var lines []string
	cur := ""
	for _, w := range words {
		if cur == "" {
			cur = w
			continue
		}
		if len(cur)+1+len(w) > 92 {
			lines = append(lines, cur)
			cur = w
			continue
		}
		cur += " " + w
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return strings.Join(lines, "\n  ")
}

// yamlString quotes a scalar that could otherwise be read as something else --
// a revision like sha256:abc is a plain scalar containing a colon, which is a
// mapping to a YAML parser.
func yamlString(s string) string {
	if strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`,") || s == "" {
		return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
	}
	return s
}

// SetRelatedWork patches the related_work block of a published record.
//
// A patch rather than a re-render, for the reason MarkSuperseded is: the body
// belongs to whoever wrote it. This exists because the two halves of publishing
// a decision are deliberately independent -- the bead does not wait on Git and
// Git does not wait on the bead (0063) -- so a record can reach Git before its
// bead exists, and then the file needs the link adding rather than the record
// needing republishing.
func SetRelatedWork(existing []byte, refs []WorkRef) ([]byte, error) {
	if len(refs) == 0 {
		return existing, nil
	}
	text := string(existing)
	if !strings.HasPrefix(text, "---\n") {
		return nil, errors.New("the file has no front matter to patch")
	}
	front, body, ok := strings.Cut(strings.TrimPrefix(text, "---\n"), "\n---\n")
	if !ok {
		return nil, errors.New("the front matter is not terminated")
	}

	var out []string
	replaced := false
	skipping := false
	for _, line := range strings.Split(front, "\n") {
		switch {
		case strings.HasPrefix(line, "related_work:"):
			out = append(out, strings.TrimRight(renderRelatedWork(refs), "\n"))
			replaced = true
			// An empty list is one line; a populated one continues into
			// indented entries that all belong to the block being replaced.
			skipping = !strings.Contains(line, "[]")
		case skipping && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "- ")):
			// Part of the old block.
		default:
			skipping = false
			out = append(out, line)
		}
	}
	if !replaced {
		out = append(out, strings.TrimRight(renderRelatedWork(refs), "\n"))
	}

	return []byte("---\n" + strings.Join(out, "\n") + "\n---\n" + body), nil
}

func renderRelatedWork(refs []WorkRef) string {
	if len(refs) == 0 {
		return "related_work: []\n"
	}
	var b strings.Builder
	b.WriteString("related_work:\n")
	for _, w := range refs {
		fmt.Fprintf(&b, "  - organisation_id: %s\n", w.OrganisationID)
		if w.ExecutionCellID != "" {
			fmt.Fprintf(&b, "    execution_cell_id: %s\n", w.ExecutionCellID)
		}
		fmt.Fprintf(&b, "    beads_database_id: %s\n", w.BeadsDatabaseID)
		fmt.Fprintf(&b, "    bead_id: %s\n", w.BeadID)
	}
	return b.String()
}

// MarkSuperseded patches the front matter of a record that has been retired.
//
// A patch rather than a re-render, and that is the whole point: the body of a
// published record is written by a person, and regenerating the file from the
// database would delete their reasoning in the name of updating two fields.
// This changes `status` and adds `superseded_by`, and touches nothing else --
// including the front matter keys it does not recognise.
func MarkSuperseded(existing []byte, successorID string) ([]byte, error) {
	if successorID == "" {
		return nil, errors.New("marking a record superseded needs its successor")
	}
	text := string(existing)
	if !strings.HasPrefix(text, "---\n") {
		return nil, errors.New("the file has no front matter to patch")
	}
	front, body, ok := strings.Cut(strings.TrimPrefix(text, "---\n"), "\n---\n")
	if !ok {
		return nil, errors.New("the front matter is not terminated")
	}

	var out []string
	sawStatus, sawSuccessor := false, false
	for _, line := range strings.Split(front, "\n") {
		switch {
		case strings.HasPrefix(line, "status:"):
			out = append(out, "status: superseded")
			sawStatus = true
		case strings.HasPrefix(line, "superseded_by:"):
			out = append(out, "superseded_by: mem-"+successorID)
			sawSuccessor = true
		default:
			out = append(out, line)
		}
	}
	if !sawSuccessor {
		out = append(out, "superseded_by: mem-"+successorID)
	}
	if !sawStatus {
		out = append(out, "status: superseded")
	}

	return []byte("---\n" + strings.Join(out, "\n") + "\n---\n" + body), nil
}
