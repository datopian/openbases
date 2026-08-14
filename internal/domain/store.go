package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/datopian/workgraph/internal/authz"
)

// ErrNotFound is returned when a record does not exist, or exists but the
// caller may not see it.
//
// The two cases are deliberately indistinguishable. Returning "forbidden" for a
// project the caller cannot see would confirm that it exists, and the name of a
// restricted client engagement is itself confidential (ADR-0013).
var ErrNotFound = errors.New("not found")

// Store reads and writes the control-plane registry.
//
// Every method that touches project-scoped data runs inside authz.WithUser, so
// row-level security applies to the query. A method that forgets returns
// nothing rather than everything, because current_app_user() is then NULL and
// every policy denies — but "fails closed" is not an excuse for forgetting.
type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// UserBySubject resolves a provider identity to an application user.
//
// This is what makes removing a user take effect immediately: with no active
// row, a caller holding a perfectly valid Access token has no session.
func (s *Store) UserBySubject(ctx context.Context, provider, subject string) (string, string, error) {
	var userID, displayName string
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id::text, u.display_name
		FROM identities i
		JOIN users u ON u.id = i.user_id
		WHERE i.provider = $1
		  AND i.subject  = $2
		  AND u.status   = 'active'`,
		provider, subject).Scan(&userID, &displayName)

	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	return userID, displayName, nil
}

// ProjectSummary is the registry view of a project.
type ProjectSummary struct {
	ID           string `json:"id"`
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	Portfolio    string `json:"portfolio,omitempty"`
	Visibility   string `json:"visibility"`
	Status       string `json:"status"`
	PrimaryOwner string `json:"primary_owner"`
	BackupOwner  string `json:"backup_owner"`
	Repositories int    `json:"repositories"`
}

// ListProjects returns the projects this user may see.
//
// There is no "all projects" query and no administrative bypass here. An
// organisation admin sees more because their grant satisfies the RLS policy,
// not because the application skips the filter for them.
func (s *Store) ListProjects(ctx context.Context, userID string) ([]ProjectSummary, error) {
	var out []ProjectSummary

	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT p.id::text, p.slug, p.name,
			       COALESCE(pf.slug, ''),
			       p.visibility, p.status,
			       po.display_name, bo.display_name,
			       (SELECT count(*) FROM project_repositories r WHERE r.project_id = p.id)
			FROM projects p
			LEFT JOIN portfolios pf ON pf.id = p.portfolio_id
			JOIN users po ON po.id = p.primary_owner_id
			JOIN users bo ON bo.id = p.backup_owner_id
			ORDER BY p.slug`)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var p ProjectSummary
			if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.Portfolio,
				&p.Visibility, &p.Status, &p.PrimaryOwner, &p.BackupOwner,
				&p.Repositories); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ProjectBySlug returns one project, or ErrNotFound when the caller may not see
// it. The caller cannot tell those apart, which is the point.
func (s *Store) ProjectBySlug(ctx context.Context, userID, slug string) (ProjectSummary, error) {
	var p ProjectSummary
	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT p.id::text, p.slug, p.name,
			       COALESCE(pf.slug, ''),
			       p.visibility, p.status,
			       po.display_name, bo.display_name,
			       (SELECT count(*) FROM project_repositories r WHERE r.project_id = p.id)
			FROM projects p
			LEFT JOIN portfolios pf ON pf.id = p.portfolio_id
			JOIN users po ON po.id = p.primary_owner_id
			JOIN users bo ON bo.id = p.backup_owner_id
			WHERE p.slug = $1`, slug).
			Scan(&p.ID, &p.Slug, &p.Name, &p.Portfolio, &p.Visibility,
				&p.Status, &p.PrimaryOwner, &p.BackupOwner, &p.Repositories)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectSummary{}, ErrNotFound
	}
	if err != nil {
		return ProjectSummary{}, fmt.Errorf("reading project: %w", err)
	}
	return p, nil
}
