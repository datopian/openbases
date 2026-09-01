// Package authz evaluates scoped, action-based permissions.
//
// Authorisation is denied unless a grant explicitly allows it. Every API query
// filters at the database or domain service layer; hiding a button in the UI is
// not authorisation (plan section 8.2).
package authz

import (
	"context"
	"errors"
)

// Action is a permission verb. The set is closed: an unrecognised action is
// denied rather than treated as unconstrained.
type Action string

const (
	OrganisationRead  Action = "organisation.read"
	ProjectRead       Action = "project.read"
	ProjectManage     Action = "project.manage"
	WorkCreate        Action = "work.create"
	WorkUpdate        Action = "work.update"
	WorkAssign        Action = "work.assign"
	AgentDispatch     Action = "agent.dispatch"
	AgentInspect      Action = "agent.inspect"
	AgentStop         Action = "agent.stop"
	RepositoryRead    Action = "repository.read"
	PullRequestCreate Action = "pull_request.create"
	PullRequestMerge  Action = "pull_request.merge"
	ApprovalDecide    Action = "approval.decide"
	DeploymentExecute Action = "deployment.execute"
	SecretManage      Action = "secret.manage"
	PolicyManage      Action = "policy.manage"
	AuditRead         Action = "audit.read"
	MarketingPublish  Action = "marketing.publish"

	// KnowledgeClassificationDowngrade permits publishing a sanitised
	// derivative at a broader visibility than its sources. It is a protected,
	// audited action (plan section 14.3).
	KnowledgeClassificationDowngrade Action = "knowledge.classification.downgrade"
	// KnowledgeReview decides on an extracted candidate: accept, edit, reject
	// or defer. Its own action rather than WorkUpdate, because an accepted
	// candidate does not become work until WP-H4 -- and rather than
	// ProjectManage, which is administration.
	KnowledgeReview Action = "knowledge.review"
)

// allActions is the closed set of recognised actions.
var allActions = map[Action]struct{}{
	OrganisationRead: {}, ProjectRead: {}, ProjectManage: {}, WorkCreate: {},
	WorkUpdate: {}, WorkAssign: {}, AgentDispatch: {}, AgentInspect: {},
	AgentStop: {}, RepositoryRead: {}, PullRequestCreate: {}, PullRequestMerge: {},
	ApprovalDecide: {}, DeploymentExecute: {}, SecretManage: {}, PolicyManage: {},
	AuditRead: {}, MarketingPublish: {}, KnowledgeClassificationDowngrade: {},
	KnowledgeReview: {},
}

// Known reports whether a is a recognised action.
func (a Action) Known() bool { _, ok := allActions[a]; return ok }

// Protected reports whether an action normally requires a durable human
// approval before execution (plan section 10.1).
func (a Action) Protected() bool {
	switch a {
	case PullRequestMerge, DeploymentExecute, SecretManage, PolicyManage,
		MarketingPublish, KnowledgeClassificationDowngrade:
		return true
	}
	return false
}

// ErrDenied is returned when a subject may not perform an action.
var ErrDenied = errors.New("permission denied")

// Request is a single authorisation question.
type Request struct {
	UserID    string
	Action    Action
	ProjectID string // empty for organisation-scoped actions
}

// Grant is a scoped permission held by a user. An empty ProjectID means the
// grant applies at organisation scope.
type Grant struct {
	UserID    string
	Action    Action
	ProjectID string
}

// Authorizer answers authorisation questions.
type Authorizer interface {
	Authorize(ctx context.Context, req Request) error
}

// GrantSet is an in-memory Authorizer used by tests and local development. The
// production implementation reads scoped role grants from PostgreSQL with
// row-level security; the deny-by-default semantics are identical.
type GrantSet struct{ grants []Grant }

// NewGrantSet builds an authorizer over an explicit list of grants.
func NewGrantSet(grants ...Grant) *GrantSet { return &GrantSet{grants: grants} }

// Authorize denies unless a grant matches exactly, or an organisation-scoped
// grant covers the requested project.
func (g *GrantSet) Authorize(_ context.Context, req Request) error {
	if req.UserID == "" {
		return ErrDenied
	}
	if !req.Action.Known() {
		// An unrecognised action is denied. Adding a permission must be a
		// deliberate change here, not an emergent property of a typo.
		return ErrDenied
	}
	for _, gr := range g.grants {
		if gr.UserID != req.UserID || gr.Action != req.Action {
			continue
		}
		if gr.ProjectID == "" || gr.ProjectID == req.ProjectID {
			return nil
		}
	}
	return ErrDenied
}
