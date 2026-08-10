// Package domain holds the first-class organisational objects of the work graph
// (plan section 2.2). These types are the shared vocabulary of the control
// plane; adapters translate provider-specific shapes into them.
package domain

import (
	"errors"
	"fmt"
	"time"
)

// Visibility is the classification of an object. Derived objects inherit the
// most restrictive classification of their sources (plan section 14.3).
type Visibility string

const (
	VisibilityInternal     Visibility = "internal"
	VisibilityConfidential Visibility = "confidential"
	VisibilityRestricted   Visibility = "restricted"
)

// rank orders visibility levels so that inheritance can be computed.
func (v Visibility) rank() int {
	switch v {
	case VisibilityInternal:
		return 1
	case VisibilityConfidential:
		return 2
	case VisibilityRestricted:
		return 3
	}
	return 0 // unknown
}

// Valid reports whether v is a recognised classification.
func (v Visibility) Valid() bool { return v.rank() > 0 }

// MoreRestrictiveThan reports whether v is stricter than other.
func (v Visibility) MoreRestrictiveThan(other Visibility) bool {
	return v.rank() > other.rank()
}

// InheritVisibility returns the most restrictive classification among sources.
// It is an error to call it with no sources: an object with no provenance has
// no basis for a classification and must be rejected rather than defaulted.
func InheritVisibility(sources ...Visibility) (Visibility, error) {
	if len(sources) == 0 {
		return "", errors.New("cannot inherit visibility: no sources")
	}
	out := VisibilityInternal
	for _, s := range sources {
		if !s.Valid() {
			return "", fmt.Errorf("invalid source visibility %q", s)
		}
		if s.MoreRestrictiveThan(out) {
			out = s
		}
	}
	return out, nil
}

// Scope is the graph layer that owns an object.
type Scope string

const (
	ScopeCompany  Scope = "company"
	ScopeProject  Scope = "project"
	ScopeFunction Scope = "function"
	ScopePersonal Scope = "personal"
)

// WorkRef is the stable identity of a work item. Beads databases are isolated
// and project prefixes can collide, so anything stored outside Beads references
// the full tuple, never the bead ID alone (plan section 7.3).
type WorkRef struct {
	OrganisationID  string `json:"organisation_id"`
	ExecutionCellID string `json:"execution_cell_id"`
	BeadsDatabaseID string `json:"beads_database_id"`
	BeadID          string `json:"bead_id"`
}

// Validate reports whether every component of the tuple is present.
func (w WorkRef) Validate() error {
	var missing []string
	if w.OrganisationID == "" {
		missing = append(missing, "organisation_id")
	}
	if w.ExecutionCellID == "" {
		missing = append(missing, "execution_cell_id")
	}
	if w.BeadsDatabaseID == "" {
		missing = append(missing, "beads_database_id")
	}
	if w.BeadID == "" {
		missing = append(missing, "bead_id")
	}
	if len(missing) > 0 {
		return fmt.Errorf("incomplete work reference, missing: %v", missing)
	}
	return nil
}

// String renders the tuple in a stable, log-safe form.
func (w WorkRef) String() string {
	return fmt.Sprintf("%s/%s/%s/%s", w.OrganisationID, w.ExecutionCellID, w.BeadsDatabaseID, w.BeadID)
}

// Kind is the business semantic of a work item, carried as a Beads label
// rather than a custom database column (plan section 7.2).
type Kind string

const (
	KindTask        Kind = "task"
	KindDecision    Kind = "decision"
	KindRisk        Kind = "risk"
	KindCommitment  Kind = "commitment"
	KindDeliverable Kind = "deliverable"
	KindCampaign    Kind = "campaign"
	KindSignal      Kind = "signal"
	KindMemory      Kind = "memory"
)

// Project is a client engagement, product initiative, campaign, or internal
// initiative. It is the unit of membership, permission, and budget.
type Project struct {
	ID              string
	Slug            string
	Name            string
	Portfolio       string
	Visibility      Visibility
	PrimaryOwner    string
	BackupOwner     string
	ExecutionCellID string
	CreatedAt       time.Time
}

// Validate enforces the invariants a project must satisfy before it is
// registered. A project without a backup owner reintroduces the single-operator
// risk the plan exists to remove (plan section 2.1).
func (p Project) Validate() error {
	var errs []error
	if p.Slug == "" {
		errs = append(errs, errors.New("slug is required"))
	}
	if !p.Visibility.Valid() {
		errs = append(errs, fmt.Errorf("invalid visibility %q", p.Visibility))
	}
	if p.PrimaryOwner == "" {
		errs = append(errs, errors.New("primary owner is required"))
	}
	if p.BackupOwner == "" {
		errs = append(errs, errors.New("backup owner is required for continuity"))
	}
	if p.PrimaryOwner != "" && p.PrimaryOwner == p.BackupOwner {
		errs = append(errs, errors.New("backup owner must differ from primary owner"))
	}
	if p.Visibility == VisibilityRestricted && p.ExecutionCellID == "" {
		errs = append(errs, errors.New("a restricted project requires its own execution cell"))
	}
	return errors.Join(errs...)
}
