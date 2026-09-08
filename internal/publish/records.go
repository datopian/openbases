package publish

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/datopian/openbases/internal/beads"
	"github.com/datopian/openbases/internal/githubapp"
	"github.com/datopian/openbases/internal/knowledge"
)

// GitHub is the part of the App client the record publisher needs.
type GitHub interface {
	InstallationToken(ctx context.Context, repositories ...string) (*githubapp.InstallationToken, error)
	BranchHead(ctx context.Context, tok *githubapp.InstallationToken, owner, repo, branch string) (string, error)
	CreateBranch(ctx context.Context, tok *githubapp.InstallationToken, owner, repo, branch, sha string) error
	PutFile(ctx context.Context, tok *githubapp.InstallationToken, owner, repo, branch, path, message string, content []byte) error
	FileContent(ctx context.Context, tok *githubapp.InstallationToken, owner, repo, ref, path string) ([]byte, string, error)
	OpenPullRequest(ctx context.Context, tok *githubapp.InstallationToken, req githubapp.OpenPullRequestRequest) (*githubapp.PullRequest, error)
	FindPullRequest(ctx context.Context, tok *githubapp.InstallationToken, owner, repo, head string) (*githubapp.PullRequest, error)
}

// PendingRecord is an accepted durable record not yet proposed in Git.
type PendingRecord struct {
	Record knowledge.Record
	// OrganisationID is the record's own, and becomes the first element of the
	// bead's identity tuple. On the row rather than resolved once per pass,
	// because deriving it from a bead that does not exist yet is circular.
	OrganisationID string
	CandidateID    string
	// The graph a decision bead belongs in, chosen by the same routing the
	// operational publisher uses.
	GraphID   string
	GraphName string
	GraphPath string
	GraphHost string
	Blocked   string
	// Which halves are already done. A record is offered while either is
	// outstanding, so the pass applies the missing one rather than repeating
	// the finished one.
	MarkdownPublished bool
	BeadPublished     bool
	// SupersedesGitPath is the file of the record this one retires, when that
	// record has one. Marked superseded in the same pull request, so the
	// correction and the retirement are one reviewable change.
	SupersedesGitPath string
}

// RecordResult counts one pass.
type RecordResult struct {
	Proposed int
	Beads    int
	// Linked counts records whose pull request was already open when their
	// bead arrived, so the file was patched rather than republished.
	Linked  int
	Blocked int
	Failed  int
}

// RecordPublisher proposes accepted durable records as Markdown pull requests,
// and gives an accepted decision its bead.
//
// Two effects rather than one, because WP-H4 asks for both: the pull request is
// where a person argues with the wording, and the bead is how the decision
// shows up in the work graph instead of only in a document nobody opens.
type RecordPublisher struct {
	DB     *sql.DB
	GitHub GitHub
	Beads  Beads
	// Owner and Repo are the repository knowledge records live in. Empty means
	// not configured, and the pass does nothing rather than guessing.
	Owner string
	Repo  string
	Base  string
	Node  string
	Log   *slog.Logger
}

// Run proposes at most limit records.
func (p *RecordPublisher) Run(ctx context.Context, limit int) (RecordResult, error) {
	if p.DB == nil {
		return RecordResult{}, errors.New("a record publisher needs a database")
	}
	// A deployment with no knowledge repository still publishes decision
	// beads. Nothing about a bead needs Git, and the state is real: the
	// GitHub App installation does not include company-workgraph, so coupling
	// the two meant an accepted decision appeared nowhere in the work graph
	// for a reason that has nothing to do with the work graph.
	markdown := p.Owner != "" && p.Repo != "" && p.GitHub != nil
	log := p.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}
	base := p.Base
	if base == "" {
		base = "main"
	}

	pending, err := p.pending(ctx, limit)
	if err != nil {
		return RecordResult{}, err
	}
	if len(pending) == 0 {
		return RecordResult{}, nil
	}

	var (
		tok  *githubapp.InstallationToken
		head string
	)
	if markdown {
		// One token for the pass, scoped to the one repository it writes to.
		// An installation token is broad by default: scoping it means a bug
		// here cannot touch a client repository.
		//
		// The BARE name, not owner/name. GitHub's scoped mint takes repository
		// names and answers an owner-qualified one with the same 422 it uses
		// for a repository the installation does not cover -- which read as a
		// missing grant for a repository the installation already had.
		tok, err = p.GitHub.InstallationToken(ctx, p.Repo)
		if err != nil {
			return RecordResult{}, fmt.Errorf("minting a token for %s/%s: %w", p.Owner, p.Repo, err)
		}
		head, err = p.GitHub.BranchHead(ctx, tok, p.Owner, p.Repo, base)
		if err != nil {
			return RecordResult{}, err
		}
	}

	var res RecordResult
	for _, r := range pending {
		if r.Blocked != "" {
			res.Blocked++
			log.Error("an accepted record cannot be published",
				"record", r.Record.ID, "type", r.Record.Type, "reason", r.Blocked)
			continue
		}

		// The bead FIRST, so a record reaching Git for the first time carries
		// the link in its front matter rather than needing it patched in.
		// Only for decisions: a constraint or a lesson is something to know,
		// not something to do, and a graph full of beads nobody can act on is
		// how a work graph stops being read.
		if r.Record.Type == "decision" && !r.BeadPublished {
			ref, err := p.decisionBead(ctx, r, log)
			switch {
			case err != nil:
				res.Failed++
				log.Error("creating the decision bead", "record", r.Record.ID, "error", err)
			case ref != nil:
				res.Beads++
				r.Record.RelatedWork = append(r.Record.RelatedWork, *ref)
			}
		}

		switch {
		case !markdown:
			// Nothing to do in Git this pass.
		case !r.MarkdownPublished:
			if err := p.propose(ctx, tok, head, base, r, log); err != nil {
				res.Failed++
				log.Error("proposing a record", "record", r.Record.ID, "error", err)
				// Deliberately after the bead: Git does not hold up the work
				// graph, and the work graph does not hold up Git.
			} else {
				res.Proposed++
			}
		case len(r.Record.RelatedWork) > 0:
			// Already proposed, and the bead has only just arrived -- the two
			// halves are independent, so this ordering is possible whenever
			// the bead failed on an earlier pass. The open pull request is
			// patched rather than the record republished.
			if err := p.linkWork(ctx, tok, base, r, log); err != nil {
				res.Failed++
				log.Error("adding the bead to a proposed record",
					"record", r.Record.ID, "error", err)
			} else {
				res.Linked++
			}
		}
	}
	return res, nil
}

// linkWork adds the bead to a record whose pull request is already open.
func (p *RecordPublisher) linkWork(ctx context.Context, tok *githubapp.InstallationToken,
	base string, r PendingRecord, log *slog.Logger) error {

	path := r.Record.Path()
	branch := recordBranch(r.Record)

	content, _, err := p.GitHub.FileContent(ctx, tok, p.Owner, p.Repo, branch, path)
	if err != nil {
		return err
	}
	patched, err := knowledge.SetRelatedWork(content, r.Record.RelatedWork)
	if err != nil {
		return err
	}
	if string(patched) == string(content) {
		return nil
	}
	if err := p.GitHub.PutFile(ctx, tok, p.Owner, p.Repo, branch, path,
		fmt.Sprintf("Link mem-%s to its bead", shortID(r.Record.ID)), patched); err != nil {
		return err
	}
	log.Info("added the bead to a record already proposed in Git",
		"record", r.Record.ID, "path", path)
	return nil
}

// recordBranch names the branch a record is proposed on.
//
// One expression rather than two: the proposal and the later patch have to
// agree on it, and a second copy is a second thing to keep in step.
func recordBranch(r knowledge.Record) string {
	return "knowledge/" + r.Type + "-" + shortID(r.ID)
}

func (p *RecordPublisher) propose(ctx context.Context, tok *githubapp.InstallationToken,
	head, base string, r PendingRecord, log *slog.Logger) error {

	content, err := r.Record.Render()
	if err != nil {
		return fmt.Errorf("rendering: %w", err)
	}
	path := r.Record.Path()
	branch := recordBranch(r.Record)

	if err := p.GitHub.CreateBranch(ctx, tok, p.Owner, p.Repo, branch, head); err != nil {
		return err
	}

	message := fmt.Sprintf("Record an accepted %s: %s", r.Record.Type, title(r.Record.Statement))
	if err := p.GitHub.PutFile(ctx, tok, p.Owner, p.Repo, branch, path, message, content); err != nil {
		return err
	}

	// The retired record, in the same pull request. Its front matter is
	// patched and its body left alone: a published record's body is written by
	// a person, and regenerating the file to change two fields would delete
	// their reasoning.
	if r.SupersedesGitPath != "" {
		if err := p.retire(ctx, tok, base, branch, r, log); err != nil {
			return fmt.Errorf("marking %s superseded: %w", r.SupersedesGitPath, err)
		}
	}

	pr, err := p.GitHub.OpenPullRequest(ctx, tok, githubapp.OpenPullRequestRequest{
		Owner: p.Owner,
		Repo:  p.Repo,
		Head:  branch,
		Base:  base,
		Title: message,
		Body:  prBody(r),
	})
	if errors.Is(err, githubapp.ErrAlreadyOpen) {
		// This pass's own earlier work: the pull request was opened and
		// something after it failed. Adopting it is the only correct move --
		// opening a second one for the same record is the duplicate this
		// whole path is built to avoid, and giving up leaves a proposal the
		// control plane does not know about.
		pr, err = p.GitHub.FindPullRequest(ctx, tok, p.Owner, p.Repo, branch)
		if err == nil && pr == nil {
			err = fmt.Errorf("GitHub reports a pull request open for %s and returns none", branch)
		}
		if err == nil {
			log.Info("adopting the pull request an earlier pass opened",
				"record", r.Record.ID, "pull_request", pr.HTMLURL)
		}
	}
	if err != nil {
		// The branch and the file survive. A retry finds the branch present,
		// rewrites the same file and opens the pull request -- which is why
		// CreateBranch treats an existing branch as success.
		return err
	}

	recorded, err := p.record(ctx, r.Record.ID, path, pr.HTMLURL)
	if err != nil {
		return err
	}
	log.Info("proposed an accepted record in Git",
		"record", r.Record.ID, "type", r.Record.Type, "path", path,
		"pull_request", pr.HTMLURL, "recorded", recorded)
	return nil
}

// retire marks the superseded record's file, on the branch the successor is on.
func (p *RecordPublisher) retire(ctx context.Context, tok *githubapp.InstallationToken,
	base, branch string, r PendingRecord, log *slog.Logger) error {

	// Read from the branch first: a retry finds the patch already applied
	// there, and reading the base would undo nothing but would rewrite the
	// file with an identical patch every pass.
	content, _, err := p.GitHub.FileContent(ctx, tok, p.Owner, p.Repo, branch, r.SupersedesGitPath)
	if err != nil {
		content, _, err = p.GitHub.FileContent(ctx, tok, p.Owner, p.Repo, base, r.SupersedesGitPath)
	}
	if err != nil {
		return err
	}

	patched, err := knowledge.MarkSuperseded(content, r.Record.ID)
	if err != nil {
		return err
	}
	if string(patched) == string(content) {
		log.Info("the superseded record is already marked",
			"path", r.SupersedesGitPath, "successor", r.Record.ID)
		return nil
	}

	return p.GitHub.PutFile(ctx, tok, p.Owner, p.Repo, branch, r.SupersedesGitPath,
		fmt.Sprintf("Mark mem-%s superseded", shortID(r.Record.Supersedes)), patched)
}

// prBody says what a reviewer is being asked to do, and admits what the file
// does not contain.
func prBody(r PendingRecord) string {
	var b strings.Builder
	b.WriteString("An accepted knowledge record, proposed as Markdown so it can be read and argued with in a diff.\n\n")
	fmt.Fprintf(&b, "- type: `%s`\n", r.Record.Type)
	fmt.Fprintf(&b, "- scope: `%s`", r.Record.Scope)
	if r.Record.Project != "" {
		fmt.Fprintf(&b, " (`%s`)", r.Record.Project)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "- classification: `%s`\n", r.Record.Visibility)
	fmt.Fprintf(&b, "- reviewer: %s\n", r.Record.Reviewer)
	fmt.Fprintf(&b, "- record: `mem-%s`\n", r.Record.ID)
	if r.Record.Supersedes != "" {
		fmt.Fprintf(&b, "- supersedes: `mem-%s`", r.Record.Supersedes)
		if r.SupersedesGitPath != "" {
			fmt.Fprintf(&b, ", marked superseded in this pull request at `%s`", r.SupersedesGitPath)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString("The Context and Consequences sections are deliberately unfinished: the statement " +
		"and its provenance are facts the control plane has, the reasoning is not, and a generated " +
		"paragraph in its place would read as somebody's thinking without being it.\n\n")
	b.WriteString("Sources are referenced by provider, revision and line, never copied, so no " +
		"transcript text is committed (ADR-0027).\n")
	return b.String()
}

// decisionBead creates the bead for an accepted decision and links it to the
// candidate the decision came from.
//
// Reuses the operational recording function, so a decision's bead is recorded
// the same way and in the same place as a task's -- one answer to "which bead
// did this become".
func (p *RecordPublisher) decisionBead(ctx context.Context, r PendingRecord, log *slog.Logger) (*knowledge.WorkRef, error) {
	switch {
	case p.Beads == nil:
		return nil, nil
	case r.CandidateID == "":
		// A hand-written decision has no candidate to hang the bead off. The
		// pull request is still the record of it; saying so beats inventing a
		// link.
		log.Info("no decision bead: the record has no candidate to attribute it to",
			"record", r.Record.ID)
		return nil, nil
	case r.GraphID == "" || r.GraphPath == "":
		log.Error("no decision bead: no provisioned graph for this record",
			"record", r.Record.ID, "project", r.Record.Project)
		return nil, nil
	case p.Node != "" && r.GraphHost != "" && r.GraphHost != p.Node:
		log.Info("no decision bead: the graph is on another host",
			"record", r.Record.ID, "graph", r.GraphName, "host", r.GraphHost)
		return nil, nil
	}

	if p.DB == nil {
		return nil, errors.New("no database to record the bead in")
	}

	ref := beads.DatabaseRef{ID: r.GraphID, Name: r.GraphName, Path: r.GraphPath}
	label := LabelPrefix + r.CandidateID
	client := p.Beads
	if r.Record.Reviewer != "" {
		client = client.WithActor(r.Record.Reviewer)
	}

	existing, err := client.ByLabel(ctx, ref, label)
	if err != nil {
		return nil, err
	}

	bead := ""
	if len(existing) > 0 {
		bead = existing[0].Ref.BeadID
	} else {
		issue := beads.Issue{
			Title:       title(r.Record.Statement),
			Description: decisionBody(r),
			Type:        "decision",
			Priority:    2,
			Labels:      []string{label, "wg-decision"},
			Assignee:    r.Record.Owner,
			ExternalRef: "wg-record-" + r.Record.ID,
		}
		if issue.Assignee == "" {
			issue.Assignee = r.Record.Reviewer
		}
		if r.Record.Project != "" {
			issue.Labels = append(issue.Labels, "wg-project-"+r.Record.Project)
		}
		created, err := client.Create(ctx, ref, issue)
		if err != nil {
			return nil, err
		}
		bead = created.BeadID
	}

	var workRef string
	if err := p.DB.QueryRowContext(ctx,
		`SELECT system_record_work_publication($1::uuid, $2::uuid, $3, $4, $5)::text`,
		r.CandidateID, r.GraphID, bead, title(r.Record.Statement), "decision").Scan(&workRef); err != nil {
		return nil, fmt.Errorf("recording decision bead %s: %w", bead, err)
	}
	log.Info("created the bead for an accepted decision",
		"record", r.Record.ID, "bead", bead, "graph", r.GraphName, "work_ref", workRef)

	// Returned so a record reaching Git in this same pass carries the link,
	// rather than being proposed with an empty related_work and patched a
	// moment later.
	return &knowledge.WorkRef{
		OrganisationID:  r.OrganisationID,
		BeadsDatabaseID: r.GraphID,
		BeadID:          bead,
	}, nil
}

func decisionBody(r PendingRecord) string {
	var b strings.Builder
	b.WriteString(r.Record.Statement)
	b.WriteString("\n\n---\nAn accepted decision.\n")
	fmt.Fprintf(&b, "Record: mem-%s\n", r.Record.ID)
	// Named only when it exists. A bead pointing at a file nobody has written
	// yet sends the reader looking for something that is not there.
	if r.MarkdownPublished {
		b.WriteString("The reasoning lives in the Markdown record.\n")
	}
	if r.Record.Reviewer != "" {
		fmt.Fprintf(&b, "Reviewed by: %s\n", r.Record.Reviewer)
	}
	fmt.Fprintf(&b, "Classification: %s\n", r.Record.Visibility)
	return b.String()
}

func shortID(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (p *RecordPublisher) record(ctx context.Context, recordID, path, prURL string) (bool, error) {
	// Refused rather than dereferenced. Run holds the same check, and this one
	// is what makes the difference visible if that ever stops being true: a
	// proposal nothing recorded is a pull request the control plane will open
	// again on the next pass.
	if p.DB == nil {
		return false, errors.New("no database to record the publication in")
	}
	var recorded bool
	err := p.DB.QueryRowContext(ctx,
		`SELECT system_record_markdown_publication($1::uuid, $2, $3)`,
		recordID, path, prURL).Scan(&recorded)
	if err != nil {
		return false, fmt.Errorf("recording the publication of %s: %w", recordID, err)
	}
	return recorded, nil
}

func (p *RecordPublisher) pending(ctx context.Context, limit int) ([]PendingRecord, error) {
	rows, err := p.DB.QueryContext(ctx, `
		SELECT record_id::text, organisation_id::text, record_type, statement, scope,
		       coalesce(project_slug,''), coalesce(function_slug,''),
		       visibility, coalesce(confidence,0), valid_from, review_after,
		       reviewed_at, coalesce(reviewer_email,''), coalesce(owner_email,''),
		       coalesce(author_email,''), human_authored,
		       coalesce(supersedes::text,''), coalesce(supersedes_git_path,''), sources,
		       related_work,
		       coalesce(candidate_id::text,''), markdown_published, bead_published,
		       coalesce(graph_id::text,''), coalesce(graph_name,''),
		       coalesce(graph_path,''), coalesce(graph_host,''),
		       coalesce(blocked_reason,'')
		  FROM system_pending_record_publications($1)`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing records awaiting a pull request: %w", err)
	}
	defer rows.Close()

	var out []PendingRecord
	for rows.Next() {
		var p PendingRecord
		var validFrom, reviewAfter sql.NullTime
		var reviewedAt sql.NullTime
		var sources, related []byte
		if err := rows.Scan(&p.Record.ID, &p.OrganisationID,
			&p.Record.Type, &p.Record.Statement, &p.Record.Scope,
			&p.Record.Project, &p.Record.Function, &p.Record.Visibility, &p.Record.Confidence,
			&validFrom, &reviewAfter, &reviewedAt, &p.Record.Reviewer, &p.Record.Owner,
			&p.Record.Author, &p.Record.HumanAuthored, &p.Record.Supersedes,
			&p.SupersedesGitPath, &sources, &related,
			&p.CandidateID, &p.MarkdownPublished, &p.BeadPublished,
			&p.GraphID, &p.GraphName, &p.GraphPath, &p.GraphHost,
			&p.Blocked); err != nil {
			return nil, err
		}
		if validFrom.Valid {
			p.Record.ValidFrom = validFrom.Time
		}
		if reviewAfter.Valid {
			p.Record.ReviewAfter = reviewAfter.Time
		}
		if reviewedAt.Valid {
			p.Record.ReviewedAt = reviewedAt.Time
		} else {
			p.Record.ReviewedAt = time.Now().UTC()
		}
		if len(sources) > 0 {
			if err := json.Unmarshal(sources, &p.Record.Sources); err != nil {
				return nil, fmt.Errorf("decoding the provenance of %s: %w", p.Record.ID, err)
			}
		}
		if len(related) > 0 {
			if err := json.Unmarshal(related, &p.Record.RelatedWork); err != nil {
				return nil, fmt.Errorf("decoding the work references of %s: %w", p.Record.ID, err)
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
