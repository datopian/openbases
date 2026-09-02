package knowledge

import (
	"strings"
	"testing"
	"time"
)

// frontMatter reads the shapes THIS renderer emits: `key: value`, a folded
// scalar whose continuation lines are indented, and lists of maps. Not a YAML
// parser, and not pretending to be one -- the repository carries three
// dependencies on purpose, and a YAML library for one assertion is not worth
// the fourth. What it does prove is the property that broke first: a value
// containing a colon has to be quoted or folded, or a reader takes it as a
// nested mapping.
func unquote(v string) string { return strings.Trim(v, `"`) }

func frontMatter(t *testing.T, rendered string) (flat map[string]string, lists map[string][]map[string]string) {
	t.Helper()
	body, ok := strings.CutPrefix(rendered, "---\n")
	if !ok {
		t.Fatalf("no front matter opener in:\n%s", rendered)
	}
	front, _, ok := strings.Cut(body, "\n---\n")
	if !ok {
		t.Fatalf("no front matter terminator in:\n%s", rendered)
	}

	flat = map[string]string{}
	lists = map[string][]map[string]string{}
	var listKey string
	var folding string
	for _, line := range strings.Split(front, "\n") {
		switch {
		case strings.HasPrefix(line, "  - "):
			if listKey == "" {
				t.Fatalf("a list item with no key: %q", line)
			}
			k, v, _ := strings.Cut(strings.TrimPrefix(line, "  - "), ": ")
			lists[listKey] = append(lists[listKey], map[string]string{k: unquote(v)})
		case strings.HasPrefix(line, "    "):
			if listKey == "" || len(lists[listKey]) == 0 {
				t.Fatalf("an indented pair outside a list: %q", line)
			}
			k, v, _ := strings.Cut(strings.TrimSpace(line), ": ")
			lists[listKey][len(lists[listKey])-1][k] = unquote(v)
		case strings.HasPrefix(line, "  "):
			if folding == "" {
				t.Fatalf("an indented line that continues nothing: %q", line)
			}
			flat[folding] = strings.TrimSpace(flat[folding] + " " + strings.TrimSpace(line))
		default:
			k, v, found := strings.Cut(line, ": ")
			if !found {
				k = strings.TrimSuffix(line, ":")
				v = ""
			}
			listKey, folding = "", ""
			switch {
			case v == ">-":
				folding = k
				flat[k] = ""
			case v == "":
				listKey = k
			default:
				flat[k] = unquote(v)
			}
		}
	}
	return flat, lists
}

func aRecord() Record {
	return Record{
		ID:         "8c1e40c9-d5ab-4ecc-b8ba-3f51f9a0ac7f",
		Type:       "decision",
		Statement:  "Use batch ingestion for phase one: streaming can wait.",
		Scope:      "project",
		Project:    "cdt",
		Visibility: "restricted",
		Confidence: 0.94,
		ValidFrom:  time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		ReviewedAt: time.Date(2026, 9, 2, 10, 30, 0, 0, time.UTC),
		Reviewer:   "monika.popova@datopian.com",
		Owner:      "joao.demenech@datopian.com",
		Sources: []Source{{
			SourceID:         "spaces/abc/transcripts/1",
			Provider:         "google-meet",
			ProviderRevision: "sha256:0badc0de",
			ExcerptRefs:      []string{"line:12", "line:13"},
		}},
	}
}

// The front matter is the contract with the control plane, so it has to parse
// as YAML -- a statement containing a colon is the obvious way for that to stop
// being true.
func TestFrontMatterReadsBack(t *testing.T) {
	out, err := aRecord().Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got, lists := frontMatter(t, string(out))
	if got["id"] != "mem-8c1e40c9-d5ab-4ecc-b8ba-3f51f9a0ac7f" {
		t.Fatalf("id = %v", got["id"])
	}
	if got["statement"] != "Use batch ingestion for phase one: streaming can wait." {
		t.Fatalf("statement = %q", got["statement"])
	}
	if got["visibility"] != "restricted" || got["project"] != "cdt" {
		t.Fatalf("classification or project lost: %v", got)
	}
	if got["status"] != "accepted" {
		t.Fatalf("status = %v", got["status"])
	}
	// A revision like sha256:... reads as a mapping unless it is quoted.
	sources := lists["sources"]
	if len(sources) != 1 {
		t.Fatalf("sources = %v", sources)
	}
	if sources[0]["provider_revision"] != "sha256:0badc0de" {
		t.Fatalf("provider revision = %q", sources[0]["provider_revision"])
	}
	if sources[0]["provider"] != "google-meet" {
		t.Fatalf("provider = %q", sources[0]["provider"])
	}
}

// WP-H4: the raw transcript is not committed. The renderer is given references
// and never text, so the only way text could appear is if somebody added a
// field for it.
func TestNoExcerptTextIsWritten(t *testing.T) {
	out, err := aRecord().Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The Record type has nowhere to put excerpt text, so the only way it
	// could appear is if a field for it were added. These are the strings a
	// transcript span would arrive as.
	for _, word := range []string{"said it", "\"text\":", "excerpt_text"} {
		if strings.Contains(string(out), word) {
			t.Fatalf("the file contains %q:\n%s", word, out)
		}
	}
	if !strings.Contains(string(out), "excerpt_refs: [line:12, line:13]") {
		t.Fatalf("the span references are missing:\n%s", out)
	}
}

// Rule 1 of RECORD-SCHEMA.md. An unattributable claim in company memory is
// worse than a missing one, because it reads as established.
func TestARecordWithNoProvenanceIsRefused(t *testing.T) {
	r := aRecord()
	r.Sources = nil
	if _, err := r.Render(); err == nil {
		t.Fatal("a record with no source and no author rendered anyway")
	}

	r.HumanAuthored = true
	r.Author = "anuar.ustayev@datopian.com"
	if _, err := r.Render(); err != nil {
		t.Fatalf("a hand-written record with a named author was refused: %v", err)
	}
}

func TestScopeMustNameWhatItScopesTo(t *testing.T) {
	r := aRecord()
	r.Project = ""
	if _, err := r.Render(); err == nil {
		t.Fatal("a project-scoped record with no project rendered anyway")
	}
	r.Scope = "function"
	r.Function = ""
	if _, err := r.Render(); err == nil {
		t.Fatal("a function-scoped record with no function rendered anyway")
	}
}

func TestPathGroupsByScope(t *testing.T) {
	r := aRecord()
	got := r.Path()
	if !strings.HasPrefix(got, "knowledge/projects/cdt/decisions/2026-09-02-") {
		t.Fatalf("path = %q", got)
	}
	if !strings.HasSuffix(got, "-8c1e40c9.md") {
		t.Fatalf("path does not end with the record id: %q", got)
	}

	r.Scope = "company"
	r.Project = ""
	if got := r.Path(); !strings.HasPrefix(got, "knowledge/company/decisions/") {
		t.Fatalf("company path = %q", got)
	}
}

// Two records accepted the same day with statements that slug identically must
// not write the same file: the second would silently replace the first.
func TestSimilarStatementsGetDifferentFiles(t *testing.T) {
	a := aRecord()
	b := aRecord()
	b.ID = "11111111-2222-3333-4444-555555555555"
	if a.Path() == b.Path() {
		t.Fatalf("both records write %q", a.Path())
	}
}

func TestTheReviewDateIsOmittedWhenThereIsNone(t *testing.T) {
	out, err := aRecord().Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(out), "review_after:") {
		t.Fatal("a decision does not expire, so it should carry no review date")
	}

	r := aRecord()
	r.Type = "fact"
	r.ReviewAfter = time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	out, err = r.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(out), "review_after: 2027-03-01") {
		t.Fatalf("a fact lost its review date:\n%s", out)
	}
}
