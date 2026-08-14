package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/datopian/workgraph/internal/authz"
	"github.com/datopian/workgraph/internal/domain"
)

// ErrNoSuchDatabase reports a Beads database that is not registered.
//
// Refusing an unregistered database is the point: it means the control plane
// only ever runs bd against graphs it knows the scope, cell and path of. An
// unknown database could belong to another trust domain.
var ErrNoSuchDatabase = errors.New("beads database is not registered")

// Registry records which Beads databases exist and where they live.
//
// Beads databases are isolated and cannot reference each other (plan section
// 2.3), so the control plane needs its own index of them — this is that index,
// and the source of the paths the adapter runs against.
type Registry struct{ db *sql.DB }

func NewRegistry(db *sql.DB) *Registry { return &Registry{db: db} }

// Register records a Beads database, or updates its recorded location.
//
// Idempotent on (organisation, name) so re-running a bootstrap is safe and the
// registry can be rebuilt from code (ADR-0015).
func (r *Registry) Register(ctx context.Context, organisationID string, db DatabaseRef, projectID string) (string, error) {
	if db.Name == "" || db.Path == "" {
		return "", errors.New("a registered database needs a name and a path")
	}

	var project any
	if projectID != "" {
		project = projectID
	}

	var id string
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO beads_databases (organisation_id, execution_cell_id, project_id, name, scope, dolt_port, path)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3::uuid, $4, $5, NULLIF($6,0), $7)
		ON CONFLICT (organisation_id, name) DO UPDATE
		    SET execution_cell_id = EXCLUDED.execution_cell_id,
		        project_id        = EXCLUDED.project_id,
		        dolt_port         = EXCLUDED.dolt_port,
		        path              = EXCLUDED.path
		RETURNING id::text`,
		organisationID, db.CellID, project, db.Name, string(db.Scope), db.DoltPort, db.Path).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("registering %s: %w", db.Name, err)
	}
	return id, nil
}

// Databases returns every registered database the caller may see, with its
// resolved path.
//
// Paths are held in the control plane rather than discovered on disk: a graph
// the registry does not know about is one the control plane must not touch.
func (r *Registry) Databases(ctx context.Context, userID string) ([]DatabaseRef, error) {
	var out []DatabaseRef
	err := authz.WithUser(ctx, r.db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT b.id::text, b.name, COALESCE(c.slug, ''), b.scope,
			       COALESCE(b.dolt_port, 0), COALESCE(b.path, '')
			FROM beads_databases b
			LEFT JOIN execution_cells c ON c.id = b.execution_cell_id
			ORDER BY b.name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d DatabaseRef
			if err := rows.Scan(&d.ID, &d.Name, &d.CellID, &d.Scope, &d.DoltPort, &d.Path); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// Link records a relationship between work in DIFFERENT Beads databases.
//
// This is the cross-graph link layer from plan section 2.3. Beads cannot
// express it, so the control plane owns it as a durable, audited edge rather
// than as text in a description that nothing can query.
func (r *Registry) Link(ctx context.Context, userID string, from, to domain.WorkRef, relation string) error {
	if err := from.Validate(); err != nil {
		return fmt.Errorf("source reference: %w", err)
	}
	if err := to.Validate(); err != nil {
		return fmt.Errorf("target reference: %w", err)
	}
	if from == to {
		return errors.New("a work item cannot link to itself")
	}

	return authz.WithUser(ctx, r.db, userID, func(tx *sql.Tx) error {
		fromID, err := upsertWorkRef(ctx, tx, from)
		if err != nil {
			return err
		}
		toID, err := upsertWorkRef(ctx, tx, to)
		if err != nil {
			return err
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO work_links (from_work_ref, to_work_ref, relation, created_by)
			VALUES ($1::uuid, $2::uuid, $3, $4::uuid)
			ON CONFLICT (from_work_ref, to_work_ref, relation) DO NOTHING`,
			fromID, toID, relation, userID)
		return err
	})
}

// LinksFrom returns the cross-graph edges leaving a work item.
func (r *Registry) LinksFrom(ctx context.Context, userID string, from domain.WorkRef) ([]CrossLink, error) {
	if err := from.Validate(); err != nil {
		return nil, err
	}

	var out []CrossLink
	err := authz.WithUser(ctx, r.db, userID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT l.relation,
			       t.organisation_id::text, COALESCE(t.execution_cell_id::text, ''),
			       t.beads_database_id::text, t.bead_id, COALESCE(t.title, '')
			FROM work_links l
			JOIN work_refs f ON f.id = l.from_work_ref
			JOIN work_refs t ON t.id = l.to_work_ref
			WHERE f.organisation_id   = $1::uuid
			  AND f.beads_database_id = $2::uuid
			  AND f.bead_id           = $3
			ORDER BY l.relation, t.bead_id`,
			from.OrganisationID, from.BeadsDatabaseID, from.BeadID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l CrossLink
			if err := rows.Scan(&l.Relation, &l.To.OrganisationID, &l.To.ExecutionCellID,
				&l.To.BeadsDatabaseID, &l.To.BeadID, &l.Title); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// CrossLink is an edge between work in different Beads databases.
type CrossLink struct {
	Relation string         `json:"relation"`
	To       domain.WorkRef `json:"to"`
	Title    string         `json:"title,omitempty"`
}

// upsertWorkRef records the identity tuple of a bead so links can point at it.
//
// The tuple is stored in full rather than the bead ID alone: prefixes collide
// between databases, and work can migrate between cells (plan section 7.3).
func upsertWorkRef(ctx context.Context, tx *sql.Tx, ref domain.WorkRef) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `
		INSERT INTO work_refs (organisation_id, execution_cell_id, beads_database_id, bead_id)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3::uuid, $4)
		ON CONFLICT (organisation_id,
		             COALESCE(execution_cell_id, '00000000-0000-0000-0000-000000000000'::uuid),
		             beads_database_id, bead_id)
		DO UPDATE SET last_seen_at = now()
		RETURNING id::text`,
		ref.OrganisationID, ref.ExecutionCellID, ref.BeadsDatabaseID, ref.BeadID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("recording work reference %s: %w", ref, err)
	}
	return id, nil
}
