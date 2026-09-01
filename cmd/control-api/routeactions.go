package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/datopian/workgraph/internal/authn"
	"github.com/datopian/workgraph/internal/authz"
)

// routeAction says what a route does, in the vocabulary internal/authz already
// defines (wg-p4h.3).
//
// Until now the HTTP layer asked one question — "is there an application user"
// — and left everything else to row-level security. That was survivable while
// the only caller was a browser driven by a person: RLS decides which ROWS you
// may see, and a person clicking a button is their own rate limit.
//
// It stops being survivable when a program holds the credential. RLS says
// nothing about whether you may DISPATCH work or MERGE a pull request, only
// about what you may read, and a token that retries will find the difference.
//
// So every route names an action. TestEveryRouteNamesAnAction enumerates the
// mux and fails when one does not, which is what stops this table drifting
// behind the routes as endpoints are added.
type routeAction struct {
	// Action is the permission verb, or "" for a route that needs none.
	Action authz.Action
	// Public marks a route that deliberately requires no scoped action:
	// identity, health, and the node paths a service token uses, which are
	// authorised by their own path-bound Access application instead.
	Public bool
	// Why records the reason for Public. Unexplained exemptions are how an
	// exemption list becomes a list of things nobody reviewed.
	Why string
}

// routeActions maps "METHOD /path" to what it requires.
//
// The key is exactly the pattern registered on the mux, so the test can compare
// the two sets directly rather than by matching prefixes.
var routeActions = map[string]routeAction{
	// Identity. Answering "who am I" to a caller who has already authenticated
	// discloses nothing they do not hold.
	"GET /v1/me": {Public: true, Why: "identity of the already-authenticated caller"},

	// Reads. These are scoped by row-level security, which is the authority on
	// which rows a caller sees; the action here is the coarser question of
	// whether this credential may read the work graph at all.
	"GET /v1/work":                   {Action: authz.OrganisationRead},
	"GET /v1/work/queue":             {Action: authz.OrganisationRead},
	"GET /v1/projects":               {Action: authz.ProjectRead},
	"GET /v1/projects/{slug}":        {Action: authz.ProjectRead},
	"GET /v1/projects/{slug}/detail": {Action: authz.ProjectRead},
	// Reading a project's Workspace events is reading the project. The rows
	// themselves are then filtered by can_read_source, so this action and the
	// policy have to agree -- naming a weaker action here would let a token
	// through to a query that returns nothing, which reads as a bug rather
	// than as a refusal.
	"GET /v1/projects/{slug}/events": {Action: authz.ProjectRead},
	// Reading the queue is reading project material; RLS then scopes the rows.
	"GET /v1/candidates": {Action: authz.ProjectRead},
	// Deciding writes to company memory, so it is a write action rather than a
	// read one -- and the policy additionally refuses a review recorded under
	// anybody but the caller.
	"POST /v1/candidates/{id}/review": {Action: authz.KnowledgeReview},
	"GET /v1/ask":                     {Action: authz.OrganisationRead},
	"GET /v1/inbox":                   {Action: authz.OrganisationRead},
	"GET /v1/inbox/branches":          {Action: authz.OrganisationRead},

	// Attention items: a person's own inbox, scoped by row-level security to
	// rows they already hold. Updating your own inbox is not an organisational
	// action, which is why these carry work.update rather than something
	// broader.
	"POST /v1/attention/{id}/snooze":   {Action: authz.WorkUpdate},
	"POST /v1/attention/{id}/delegate": {Action: authz.WorkAssign},
	"POST /v1/attention/{id}/resolve":  {Action: authz.WorkUpdate},

	// The event log. organisation.read rather than something narrower: the
	// stream spans projects, and row-level security is what decides which
	// events a caller actually sees.
	"GET /v1/events":        {Action: authz.OrganisationRead},
	"GET /v1/events/stream": {Action: authz.OrganisationRead},

	// Writes that spend money or change the graph.
	"POST /v1/work/plan":            {Action: authz.WorkCreate},
	"POST /v1/work/{bead}/dispatch": {Action: authz.AgentDispatch},

	// Deciding an approval. A token can never reach this: approval.decide is in
	// tokens.Ungrantable and refused by a CHECK constraint, so no scope set
	// contains it and the intersection is always empty. Named here anyway, so
	// the route is not silently exempt and the test can assert the refusal.
	"POST /v1/approvals/{id}/decide": {Action: authz.ApprovalDecide},

	// Managing tokens. Guarded additionally by ViaToken(): a token may not mint
	// or revoke tokens whatever its scopes say, because a leaked credential
	// that can produce its own successor makes revocation meaningless.
	"POST /v1/tokens":        {Public: true, Why: "session-only, enforced by ViaToken() rather than a scope"},
	"GET /v1/tokens":         {Public: true, Why: "session-only, enforced by ViaToken() rather than a scope"},
	"DELETE /v1/tokens/{id}": {Public: true, Why: "session-only, enforced by ViaToken() rather than a scope"},

	// The execution node's own path. These are authorised by per-path Access
	// applications bound to the cells service token, and a service token has no
	// application user at all — internal/domain/resolver.go leaves UserID empty
	// deliberately, so a user-scoped action check would refuse every one of
	// them. Their authorisation is the Access application, not this table.
	"POST /v1/node/work/claim":                        {Public: true, Why: "cells service token, path-bound Access application"},
	"POST /v1/node/work/{id}/result":                  {Public: true, Why: "cells service token, path-bound Access application"},
	"POST /v1/node/work/project":                      {Public: true, Why: "cells service token, path-bound Access application"},
	"POST /v1/agent-health":                           {Public: true, Why: "cells service token, path-bound Access application"},
	"GET /v1/budget/check":                            {Public: true, Why: "cells service token, path-bound Access application"},
	"POST /v1/integrations/github/installation-token": {Public: true, Why: "cells service token, path-bound Access application"},
}

// checkRouteAction decides whether a caller's CREDENTIAL permits a route.
//
// This is deliberately only half of authorisation, and the half that is new.
// The other half — whether the PERSON holds the grant — needs a role-to-action
// mapping that does not exist yet: the roles table carries names and
// descriptions and nothing connects them to authz's actions. That is filed
// separately, and until it lands a person's reach is bounded by row-level
// security exactly as it is today.
//
// So this narrows tokens and leaves sessions where they were. A token can only
// ever be narrower than its owner; it can never be wider.
func checkRouteAction(pattern string, id authn.Identity) (ok bool, reason string) {
	ra, known := routeActions[pattern]
	if !known {
		// Fail closed. An unlisted route is a route nobody decided about, and
		// the test below exists so this is never reached in practice.
		return false, "route_not_declared"
	}
	if ra.Public || ra.Action == "" {
		return true, ""
	}
	if !id.Permits(string(ra.Action)) {
		return false, "token_scope_insufficient"
	}
	return true, ""
}

// actionFor returns the action a route requires, or "" when it requires none.
func actionFor(pattern string) authz.Action {
	ra, ok := routeActions[pattern]
	if !ok || ra.Public {
		return ""
	}
	return ra.Action
}

// declaredRoutes returns the patterns this table covers, sorted.
func declaredRoutes() []string {
	out := make([]string, 0, len(routeActions))
	for k := range routeActions {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// describeRoute renders one entry, for a test failure that says what to do.
func describeRoute(pattern string) string {
	ra, ok := routeActions[pattern]
	switch {
	case !ok:
		return fmt.Sprintf("%s: NOT DECLARED — add it to routeActions", pattern)
	case ra.Public:
		return fmt.Sprintf("%s: no action (%s)", pattern, ra.Why)
	default:
		return fmt.Sprintf("%s: %s", pattern, ra.Action)
	}
}

// patternOf reconstructs the mux pattern a request matched, so the middleware
// can look the route up. Go 1.22's ServeMux exposes it on the request.
func patternOf(r *http.Request) string {
	return strings.TrimSpace(r.Pattern)
}

// spendsMoney reports whether a route can cause model inference to happen.
//
// The daily spend cap gates only these. Refusing a read because a dispatch
// budget is exhausted would make an exhausted token useless for the one thing
// its owner most needs next — finding out what it spent the money on.
func spendsMoney(pattern string) bool {
	switch pattern {
	case "POST /v1/work/plan", "POST /v1/work/{bead}/dispatch":
		return true
	}
	return false
}
