package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/datopian/openbases/internal/authz"
)

// Attaching repositories to a project (wg-m6p).
//
// project_repositories held twelve rows on staging and had no endpoint of any
// kind, so the only way to attach a repository was to write the INSERT by hand
// on the node.

// Repository is one attached repository.
type Repository struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Provider      string `json:"provider"`
	DefaultBranch string `json:"default_branch"`
}

// namePattern is GitHub's own rule for an owner or repository name: letters,
// digits, and the three punctuation characters, no leading separator.
//
// Enforced here so "datopian/openbases.git" or a pasted URL is refused with a
// message, rather than stored and then failing later against the API with
// something less obvious.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ParseFullName splits "owner/name" and refuses anything else.
//
// Accepts a full GitHub URL too, because that is what a person has in their
// clipboard. Refusing it would be technically defensible and mildly hostile.
func ParseFullName(s string) (owner, name string, err error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://github.com/")
	s = strings.TrimPrefix(s, "git@github.com:")
	s = strings.TrimSuffix(s, ".git")
	s = strings.Trim(s, "/")

	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("%w: %q is not owner/name", ErrInvalid, s)
	}
	if !namePattern.MatchString(parts[0]) || !namePattern.MatchString(parts[1]) {
		return "", "", fmt.Errorf("%w: %q is not a usable owner/name", ErrInvalid, s)
	}
	return parts[0], parts[1], nil
}

// ListRepositories returns the repositories attached to a project.
func (s *Store) ListRepositories(ctx context.Context, userID, slug string) ([]Repository, error) {
	out := []Repository{}
	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		id, err := projectIDBySlug(ctx, tx, slug)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT owner, name, provider, default_branch
			  FROM project_repositories
			 WHERE project_id = $1::uuid
			 ORDER BY owner, name`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Repository
			if err := rows.Scan(&r.Owner, &r.Name, &r.Provider, &r.DefaultBranch); err != nil {
				return err
			}
			r.FullName = r.Owner + "/" + r.Name
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AttachResult is what happened to one requested repository.
//
// Reported per repository rather than as a single status, because attaching six
// at once is the point and "one of them was already attached elsewhere" must
// not discard the other five or hide which one failed.
type AttachResult struct {
	FullName string `json:"full_name"`
	// Status is "attached", "already_attached", or "taken".
	Status string `json:"status"`
	// Note is what else the caller needs to know, in a sentence.
	//
	// Used for the empty-repository case: a repository with no commits cannot
	// be checked out, so it is initialised with a first commit and the caller
	// is told. When that fails the attachment still stands -- it is database
	// state and it succeeded -- and the note says what to do instead, because
	// otherwise the failure surfaces much later as a dispatch that cannot find
	// a rig.
	Note string `json:"note,omitempty"`
	// TakenBy names the project holding it, when Status is "taken" and the
	// caller may see that project. Empty when they may not: the slug of a
	// restricted engagement is itself confidential (ADR-0013), so the refusal
	// stays but the name does not.
	TakenBy string `json:"taken_by,omitempty"`
}

// AttachRepositories attaches repositories to a project.
//
// UNIQUE (provider, owner, name) makes a repository belong to at most one
// project, so attaching one that is already elsewhere is a conflict rather than
// a move. It is reported and skipped, never silently reassigned: moving a
// repository between projects changes who can see its pull requests, and that
// is not a side effect of an attach call.
func (s *Store) AttachRepositories(ctx context.Context, userID, slug string, fullNames []string) ([]AttachResult, error) {
	if len(fullNames) == 0 {
		return nil, fmt.Errorf("%w: name at least one repository", ErrInvalid)
	}

	type want struct{ owner, name string }
	wants := make([]want, 0, len(fullNames))
	seen := map[string]bool{}
	for _, fn := range fullNames {
		owner, name, err := ParseFullName(fn)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(owner + "/" + name)
		if seen[key] {
			// Refused rather than deduplicated. A caller who listed the same
			// repository twice has made a mistake somewhere upstream, and
			// quietly collapsing it hides that.
			return nil, fmt.Errorf("%w: %s is listed twice", ErrInvalid, owner+"/"+name)
		}
		seen[key] = true
		wants = append(wants, want{owner, name})
	}

	out := make([]AttachResult, 0, len(wants))
	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		id, err := projectIDBySlug(ctx, tx, slug)
		if err != nil {
			return err
		}
		for _, w := range wants {
			res := AttachResult{FullName: w.owner + "/" + w.name}

			// A savepoint per repository, so a conflict on one does not abort
			// the transaction and take the others with it. Without this the
			// first conflict poisons the tx and every later INSERT fails with
			// "current transaction is aborted".
			if _, err := tx.ExecContext(ctx, `SAVEPOINT attach`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO project_repositories (project_id, provider, owner, name)
				VALUES ($1::uuid, 'github', $2, $3)`, id, w.owner, w.name)
			switch {
			case err == nil:
				res.Status = "attached"
				if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT attach`); err != nil {
					return err
				}
			case isUniqueViolation(err):
				if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT attach`); err != nil {
					return err
				}
				var holder sql.NullString
				var same bool
				if err := tx.QueryRowContext(ctx, `
					SELECT p.slug, (r.project_id = $1::uuid)
					  FROM project_repositories r
					  LEFT JOIN projects p ON p.id = r.project_id
					 WHERE r.provider = 'github' AND r.owner = $2 AND r.name = $3`,
					id, w.owner, w.name).Scan(&holder, &same); err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				switch {
				case same:
					res.Status = "already_attached"
				default:
					res.Status = "taken"
					// NULL when row-level security hides the holding project.
					// The conflict is still reported; only the name is withheld.
					res.TakenBy = holder.String
				}
			default:
				return err
			}
			out = append(out, res)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DetachRepository removes one repository from a project.
func (s *Store) DetachRepository(ctx context.Context, userID, slug, owner, name string) error {
	owner, name, err := ParseFullName(owner + "/" + name)
	if err != nil {
		return err
	}
	return authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		id, err := projectIDBySlug(ctx, tx, slug)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			DELETE FROM project_repositories
			 WHERE project_id = $1::uuid AND provider = 'github' AND owner = $2 AND name = $3`,
			id, owner, name)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Indistinguishable from "attached to a project you cannot see",
			// deliberately, for the same reason ErrNotFound is.
			return ErrNotFound
		}
		return nil
	})
}

// SetRepositoryCheck records the command that says whether a change to a
// repository works, or clears it when command is empty.
//
// Written through authz.WithUser like every other project write, so the same
// membership decides it: somebody who may attach a repository may say how it is
// checked. That matters more than it looks -- the command runs code from the
// repository as the cell user, so setting one is a privileged act and not a
// piece of metadata.
func (s *Store) SetRepositoryCheck(ctx context.Context, userID, slug, owner, name, command string) error {
	owner, name, err := ParseFullName(owner + "/" + name)
	if err != nil {
		return err
	}
	command = strings.TrimSpace(command)
	return authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		id, err := projectIDBySlug(ctx, tx, slug)
		if err != nil {
			return err
		}
		var value any
		if command != "" {
			value = command
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE project_repositories SET check_command = $4
			 WHERE project_id = $1::uuid AND provider = 'github' AND owner = $2 AND name = $3`,
			id, owner, name, value)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetProjectOwners changes a project's primary and backup owner, by email.
//
// Ownership is runtime data, not schema: it must not be baked into a migration
// (the repository is public and an owner's email is a business record the
// disclosure guard refuses), so this is the endpoint that changes it. The
// project row is writable under RLS by anyone who can read the project; the
// route gates this to project.manage, which is the control that matters.
//
// Both owners in one UPDATE: the backup_owner_differs CHECK refuses a moment
// where primary and backup are the same person, so a two-statement version
// would fail on the first. Memberships follow the owners, because
// project_memberships is what authorisation reads and it must agree with the
// accountability record (0049_pilot_operators made this same pairing).
func (s *Store) SetProjectOwners(ctx context.Context, userID, slug, primaryEmail, backupEmail string) error {
	primaryEmail = strings.TrimSpace(primaryEmail)
	backupEmail = strings.TrimSpace(backupEmail)
	if primaryEmail == "" || backupEmail == "" {
		return fmt.Errorf("%w: a primary and a backup owner email are both required", ErrInvalid)
	}
	if strings.EqualFold(primaryEmail, backupEmail) {
		return fmt.Errorf("%w: the backup owner must be a different person from the primary owner", ErrInvalid)
	}
	return authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		id, err := projectIDBySlug(ctx, tx, slug)
		if err != nil {
			return err
		}
		primaryID, err := userIDByEmail(ctx, tx, primaryEmail)
		if err != nil {
			return err
		}
		backupID, err := userIDByEmail(ctx, tx, backupEmail)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE projects
			   SET primary_owner_id = $2::uuid,
			       backup_owner_id  = $3::uuid,
			       updated_at = now()
			 WHERE id = $1::uuid`, id, primaryID, backupID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM project_memberships
			 WHERE project_id = $1::uuid
			   AND role_name IN ('project_lead', 'backup_operator')`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO project_memberships (project_id, user_id, role_name)
			VALUES ($1::uuid, $2::uuid, 'project_lead'),
			       ($1::uuid, $3::uuid, 'backup_operator')
			ON CONFLICT DO NOTHING`, id, primaryID, backupID); err != nil {
			return err
		}
		return nil
	})
}

// userIDByEmail resolves a user by primary email, case-insensitively (there is
// a unique index on lower(primary_email)). A miss is ErrInvalid, not ErrNotFound:
// the project was found, the email was the bad input.
func userIDByEmail(ctx context.Context, tx *sql.Tx, email string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT id::text FROM users WHERE lower(primary_email) = lower($1)`,
		strings.TrimSpace(email)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: no user with the email %q", ErrInvalid, email)
	}
	return id, err
}

// SetProjectCell assigns the project to an execution cell, by cell slug.
//
// A repository only becomes a rig the node provisions when its project's
// execution_cell_id matches the cell -- system_rigs_wanted joins on it. So a
// project that has a repository but no cell (or a different cell) has its repo
// silently ignored by every cell: no rig, no clone, "the repo isn't being
// picked up" (the jopacc case). This is how a project is pointed at the cell
// that should run it.
func (s *Store) SetProjectCell(ctx context.Context, userID, slug, cell string) error {
	cell = strings.TrimSpace(cell)
	if cell == "" {
		return fmt.Errorf("%w: an execution cell is required", ErrInvalid)
	}
	return authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		id, err := projectIDBySlug(ctx, tx, slug)
		if err != nil {
			return err
		}
		var cellID string
		err = tx.QueryRowContext(ctx,
			`SELECT id::text FROM execution_cells WHERE slug = $1`, cell).Scan(&cellID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: no execution cell with the slug %q", ErrInvalid, cell)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE projects
			   SET execution_cell_id = $2::uuid, updated_at = now()
			 WHERE id = $1::uuid`, id, cellID); err != nil {
			return err
		}
		return nil
	})
}

// projectIDBySlug resolves a project the caller may see.
func projectIDBySlug(ctx context.Context, tx *sql.Tx, slug string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT id::text FROM projects WHERE slug = $1`, strings.TrimSpace(slug)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}
