package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/datopian/openbases/internal/authz"
)

// Creating a project, and attaching repositories to it (wg-1dm, wg-m6p).
//
// Both existed in the schema and in no interface: every project and all twelve
// repository rows on staging were written by hand with psql on the node, which
// is not a workflow anybody can be asked to use twice.

// ErrConflict is returned when a name is already taken.
//
// Separate from ErrNotFound because the answers differ: a caller who picked a
// taken slug should pick another one, and telling them "not found" would send
// them looking for a bug instead.
var ErrConflict = errors.New("already exists")

// ErrInvalid is returned for input the schema would refuse.
//
// Checked here rather than left to the constraint so the message names the
// field. A CHECK violation surfacing through the driver is accurate and
// useless: "projects_visibility_check" tells a person nothing about what to
// type instead.
var ErrInvalid = errors.New("invalid")

// slugPattern is what a slug may contain.
//
// Deliberately narrower than the column, which is bare text. A slug ends up in
// URLs, branch names, systemd unit names and Beads prefixes; the place to
// refuse a space is at the point somebody types it, not in whichever of those
// breaks first.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// NewProject is a request to create one.
type NewProject struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Portfolio  string `json:"portfolio,omitempty"`
	Objective  string `json:"objective,omitempty"`
	Visibility string `json:"visibility,omitempty"`
	// BackupOwner is an email address. Mandatory, and the reason is in the
	// schema: backup_owner_differs makes full-cycle ownership impossible to
	// vest in one person, so there is no sensible default the caller could
	// omit. The primary owner defaults to the caller, which is the common case
	// and the thing that makes creation quick.
	BackupOwner string `json:"backup_owner"`
	// PrimaryOwner is an email address, and defaults to the caller.
	PrimaryOwner string `json:"primary_owner,omitempty"`
	// Cell is an execution cell slug. Required for a restricted project, by
	// the restricted_requires_cell constraint.
	Cell string `json:"cell,omitempty"`
}

// Validate refuses what the schema would refuse, with a message that names the
// field. It does not check existence — that needs the database.
func (n *NewProject) Validate() error {
	n.Slug = strings.TrimSpace(strings.ToLower(n.Slug))
	n.Name = strings.TrimSpace(n.Name)
	n.Portfolio = strings.TrimSpace(strings.ToLower(n.Portfolio))
	n.Objective = strings.TrimSpace(n.Objective)
	n.Cell = strings.TrimSpace(strings.ToLower(n.Cell))
	n.BackupOwner = strings.TrimSpace(strings.ToLower(n.BackupOwner))
	n.PrimaryOwner = strings.TrimSpace(strings.ToLower(n.PrimaryOwner))
	if n.Visibility == "" {
		n.Visibility = "internal"
	}
	n.Visibility = strings.TrimSpace(strings.ToLower(n.Visibility))

	switch {
	case n.Slug == "":
		return fmt.Errorf("%w: a slug is required", ErrInvalid)
	case !slugPattern.MatchString(n.Slug):
		return fmt.Errorf("%w: a slug is lower-case letters, digits and single hyphens, so %q will not do",
			ErrInvalid, n.Slug)
	case n.Name == "":
		return fmt.Errorf("%w: a name is required", ErrInvalid)
	case n.BackupOwner == "":
		return fmt.Errorf("%w: a backup owner is required, and must not be the primary owner: "+
			"the schema refuses full-cycle ownership by one person", ErrInvalid)
	}
	switch n.Visibility {
	case "internal", "confidential", "restricted":
	default:
		return fmt.Errorf("%w: visibility is internal, confidential or restricted, not %q",
			ErrInvalid, n.Visibility)
	}
	if n.Visibility == "restricted" && n.Cell == "" {
		return fmt.Errorf("%w: a restricted project runs in its own execution cell, so one must be named",
			ErrInvalid)
	}
	if n.PrimaryOwner != "" && n.PrimaryOwner == n.BackupOwner {
		return fmt.Errorf("%w: the backup owner must differ from the primary owner", ErrInvalid)
	}
	return nil
}

// CreateProject creates a project and makes both owners members of it.
//
// The memberships are not decoration. can_read_project() grants access by
// membership or by an organisation-wide role, so a project created without them
// is invisible to its own owners unless they happen to be administrators —
// which is how a project can be created successfully and then not appear.
func (s *Store) CreateProject(ctx context.Context, userID string, n NewProject) (ProjectSummary, error) {
	if err := n.Validate(); err != nil {
		return ProjectSummary{}, err
	}

	var out ProjectSummary
	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		var orgID string
		if err := tx.QueryRowContext(ctx,
			`SELECT organisation_id::text FROM users WHERE id = $1::uuid`, userID).Scan(&orgID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: the caller is not an application user", ErrInvalid)
			}
			return err
		}

		primary := userID
		if n.PrimaryOwner != "" {
			id, err := userByEmail(ctx, tx, orgID, n.PrimaryOwner)
			if err != nil {
				return err
			}
			primary = id
		}
		backup, err := userByEmail(ctx, tx, orgID, n.BackupOwner)
		if err != nil {
			return err
		}
		// Checked here as well as in Validate, because Validate compares the
		// email addresses it was given and this compares the users they
		// resolved to. Two addresses for one person would pass the first and
		// hit backup_owner_differs as a constraint violation.
		if backup == primary {
			return fmt.Errorf("%w: the backup owner resolves to the same person as the primary owner", ErrInvalid)
		}

		portfolio, err := optionalIDBySlug(ctx, tx, `portfolios`, orgID, n.Portfolio)
		if err != nil {
			return err
		}
		cell, err := cellIDBySlug(ctx, tx, n.Cell)
		if err != nil {
			return err
		}

		var id string
		err = tx.QueryRowContext(ctx, `
			INSERT INTO projects
			    (organisation_id, portfolio_id, slug, name, objective, visibility,
			     primary_owner_id, backup_owner_id, execution_cell_id)
			VALUES ($1::uuid, $2::uuid, $3, $4, nullif($5,''), $6,
			        $7::uuid, $8::uuid, $9::uuid)
			RETURNING id::text`,
			orgID, portfolio, n.Slug, n.Name, n.Objective, n.Visibility,
			primary, backup, cell).Scan(&id)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: a project with the slug %q", ErrConflict, n.Slug)
		}
		if err != nil {
			return err
		}

		// Both owners, so the project is visible to the people responsible for
		// it rather than only to administrators.
		//
		// role_name, not role, and the values come from the roles table, which
		// project_memberships references. The first draft of this wrote
		// ('owner', 'backup_owner') into a column called role; both were wrong
		// and the integration test against the real schema is what said so.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO project_memberships (project_id, user_id, role_name)
			VALUES ($1::uuid, $2::uuid, 'project_lead'),
			       ($1::uuid, $3::uuid, 'backup_operator')
			ON CONFLICT DO NOTHING`, id, primary, backup); err != nil {
			return err
		}

		return tx.QueryRowContext(ctx, `
			SELECT p.id::text, p.slug, p.name, COALESCE(pf.slug,''),
			       p.visibility, p.status, po.display_name, bo.display_name, 0
			  FROM projects p
			  LEFT JOIN portfolios pf ON pf.id = p.portfolio_id
			  JOIN users po ON po.id = p.primary_owner_id
			  JOIN users bo ON bo.id = p.backup_owner_id
			 WHERE p.id = $1::uuid`, id).Scan(
			&out.ID, &out.Slug, &out.Name, &out.Portfolio,
			&out.Visibility, &out.Status, &out.PrimaryOwner, &out.BackupOwner, &out.Repositories)
	})
	if err != nil {
		return ProjectSummary{}, err
	}
	return out, nil
}

// userByEmail resolves an address within the caller's organisation.
func userByEmail(ctx context.Context, tx *sql.Tx, orgID, email string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT id::text FROM users
		  WHERE organisation_id = $1::uuid AND lower(primary_email) = $2 AND status = 'active'`,
		orgID, email).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// Named, because the caller typed it. An address that is not a user is
		// almost always a typo or somebody who has not signed in yet, and both
		// are fixed by the person reading the message.
		return "", fmt.Errorf("%w: no active user has the address %q", ErrInvalid, email)
	}
	return id, err
}

// optionalIDBySlug resolves a slug in one of the organisation-scoped lookup
// tables, or returns a NULL for an empty slug.
func optionalIDBySlug(ctx context.Context, tx *sql.Tx, table, orgID, slug string) (any, error) {
	if slug == "" {
		return nil, nil
	}
	// table is not caller input: it comes from a literal at each call site.
	var id string
	err := tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id::text FROM %s WHERE organisation_id = $1::uuid AND slug = $2`, table),
		orgID, slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no %s has the slug %q", ErrInvalid, strings.TrimSuffix(table, "s"), slug)
	}
	return id, err
}

// cellIDBySlug resolves an execution cell, which is unique globally rather than
// per organisation.
//
// An empty slug takes the deployment's shared cell. Before this, it took NULL:
// a project created without naming a cell had nowhere to run, dispatch found no
// rig, and the work sat there. That is what happened to msf -- eight beads filed
// and undispatchable, because the field a caller is not required to fill decided
// whether the project worked at all.
//
// The alternative was to require a cell from every caller. That pushes an
// infrastructure decision onto whoever is creating a project, and the honest
// answer for internal and open-source work is "the shared one" almost every
// time. Restricted projects are already refused above unless they name their
// own, which is the case where the decision genuinely belongs to a person.
func cellIDBySlug(ctx context.Context, tx *sql.Tx, slug string) (any, error) {
	if slug == "" {
		var def string
		if err := tx.QueryRowContext(ctx, `SELECT system_default_cell()`).Scan(&def); err != nil {
			return nil, err
		}
		if def == "" {
			// No shared cell, or more than one, so there is nothing to default
			// to. Naming the candidates is the difference between a caller
			// fixing it in one step and a caller guessing.
			var options string
			_ = tx.QueryRowContext(ctx,
				`SELECT coalesce(string_agg(slug, ', ' ORDER BY slug), '(none)')
				   FROM execution_cells`).Scan(&options)
			return nil, fmt.Errorf("%w: this deployment has no single shared execution cell, "+
				"so a project must name one. Available cells: %s. A shared cell is "+
				"declared in the deployment's execution_cells configuration",
				ErrInvalid, options)
		}
		slug = def
	}
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT id::text FROM execution_cells WHERE slug = $1`, slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no execution cell has the slug %q", ErrInvalid, slug)
	}
	return id, err
}

// isUniqueViolation reports whether err is a duplicate-key error.
//
// Matched on the typed SQLSTATE rather than on the error text. Substring
// matching would appear to work — pgx does put the code in the message — and
// would quietly stop working the day a message is reworded, turning a 409 that
// tells the caller to pick another slug back into a 500 that tells them
// nothing. That is the failure this codebase just spent a fix on in wg-bof.
func isUniqueViolation(err error) bool {
	// 23505 spelled out rather than pulled from github.com/jackc/pgerrcode: one
	// constant is not worth a module in go.mod and another line in the pin
	// check. The typed error is the part that matters.
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
