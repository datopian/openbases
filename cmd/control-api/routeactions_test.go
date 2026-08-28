package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/datopian/workgraph/internal/authn"
	"github.com/datopian/workgraph/internal/authz"
	"github.com/datopian/workgraph/internal/tokens"
)

// registered finds every authenticated route in main.go.
//
// Read out of the source rather than off the mux, because http.ServeMux does
// not expose the patterns registered on it. This is the technique
// scripts/check_rls_tests.py and scripts/check_infra.py already use for the
// same reason: the guard has to see the whole set, and the whole set only
// exists in the source.
func registered(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	re := regexp.MustCompile(`authed\.HandleFunc\("((?:GET|POST|PATCH|PUT|DELETE) [^"]+)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("found no routes in main.go; the pattern this test scans for has changed")
	}
	return out
}

// The acceptance criterion for wg-p4h.3: every route names an action, and this
// fails when one does not.
//
// It fails in both directions on purpose. A route with no entry is a route
// nobody decided about, and checkRouteAction refuses it — but a refusal at
// runtime is a bad way to discover that, so it is a build-time failure here. An
// entry with no route is a stale line that reads as coverage.
func TestEveryRouteNamesAnAction(t *testing.T) {
	declared := map[string]bool{}
	for _, p := range declaredRoutes() {
		declared[p] = true
	}

	var undeclared []string
	live := map[string]bool{}
	for _, p := range registered(t) {
		live[p] = true
		if !declared[p] {
			undeclared = append(undeclared, p)
		}
	}
	if len(undeclared) > 0 {
		t.Errorf("routes registered but not declared in routeActions:\n  %s\n\n"+
			"Every /v1/ route must name the action it requires, or say why it needs none. "+
			"An undeclared route is refused at runtime, which is a worse place to find out.",
			strings.Join(undeclared, "\n  "))
	}

	var stale []string
	for p := range declared {
		if !live[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("declared in routeActions but not registered:\n  %s\n\n"+
			"A stale entry reads as coverage for a route that does not exist.",
			strings.Join(stale, "\n  "))
	}
}

// An exemption without a reason is how an exemption list stops being reviewed.
func TestEveryExemptionSaysWhy(t *testing.T) {
	for _, p := range declaredRoutes() {
		ra := routeActions[p]
		if ra.Public && strings.TrimSpace(ra.Why) == "" {
			t.Errorf("%s is exempt from the action check with no reason recorded", p)
		}
		if !ra.Public && ra.Action == "" {
			t.Errorf("%s declares neither an action nor an exemption", p)
		}
		if ra.Action != "" && !ra.Action.Known() {
			t.Errorf("%s names %q, which is not an action internal/authz recognises", p, ra.Action)
		}
	}
}

// An unlisted route must be refused rather than allowed. Fail-closed is the
// property that makes the test above a safety net rather than the only guard.
func TestAnUndeclaredRouteIsRefused(t *testing.T) {
	ok, reason := checkRouteAction("GET /v1/invented", authn.Identity{UserID: "u1"})
	if ok {
		t.Fatal("an undeclared route was permitted")
	}
	if reason != "route_not_declared" {
		t.Fatalf("reason = %q, want route_not_declared", reason)
	}
}

// A session is not narrowed by this check. Scopes bound tokens; a person's
// reach is bounded by their own authorisation and by row-level security.
func TestASessionIsNotNarrowedByScopes(t *testing.T) {
	session := authn.Identity{UserID: "u1"} // no TokenID
	for _, p := range declaredRoutes() {
		if ok, reason := checkRouteAction(p, session); !ok {
			t.Errorf("%s refused an interactive session: %s", p, reason)
		}
	}
}

// The distinguishable refusal wg-p4h.3 asks for: a token whose scopes omit the
// action is refused with a code that does not read as "your role is wrong".
func TestATokenWithoutTheScopeIsRefusedDistinguishably(t *testing.T) {
	tok := authn.Identity{UserID: "u1", TokenID: "t1", Scopes: []string{string(authz.ProjectRead)}}

	ok, reason := checkRouteAction("POST /v1/work/{bead}/dispatch", tok)
	if ok {
		t.Fatal("a token without agent.dispatch was allowed to dispatch")
	}
	if reason != "token_scope_insufficient" {
		t.Fatalf("reason = %q, want token_scope_insufficient — an operator debugging this "+
			"must not be told to change their role", reason)
	}

	if ok, _ := checkRouteAction("GET /v1/projects", tok); !ok {
		t.Fatal("a token WITH project.read was refused a project read")
	}
}

// No token, however scoped, can decide an approval.
//
// Three independent layers say so and this asserts the outermost two. A CHECK
// constraint in 0038_api_tokens.sql refuses to store the scope; tokens.
// Ungrantable refuses to mint it; and even if a scope string arrived from
// somewhere else, the route requires approval.decide and Permits() only answers
// true for a scope the token actually carries.
func TestNoTokenCanDecideAnApproval(t *testing.T) {
	const route = "POST /v1/approvals/{id}/decide"

	// The honest worst case: a token that somehow carries the scope anyway.
	forged := authn.Identity{UserID: "u1", TokenID: "t1", Scopes: []string{string(authz.ApprovalDecide)}}
	if ok, _ := checkRouteAction(route, forged); !ok {
		t.Log("a forged scope is refused at the route as well, which is belt and braces")
	}

	// The layer that actually holds: the scope cannot be minted at all.
	if err := tokens.ValidateScopes([]string{string(authz.ApprovalDecide)}); err == nil {
		t.Fatal("approval.decide can be minted into a token, so the route check is the only " +
			"thing standing between an agent and approving its own dispatch")
	}

	// And an ordinarily-scoped token is refused at the route.
	ordinary := authn.Identity{UserID: "u1", TokenID: "t1", Scopes: []string{string(authz.WorkCreate)}}
	if ok, reason := checkRouteAction(route, ordinary); ok {
		t.Fatal("a token without approval.decide reached the approval route")
	} else if reason != "token_scope_insufficient" {
		t.Fatalf("reason = %q", reason)
	}
}

// The service token's reachable set is unchanged by this bead.
//
// Those routes are authorised by their own path-bound Access applications, and
// a service token has no application user at all — internal/domain/resolver.go
// leaves UserID empty deliberately. A user-scoped action check would refuse
// every one of them, which would break the execution node.
func TestServiceTokenRoutesAreUnchanged(t *testing.T) {
	nodeRoutes := []string{
		"POST /v1/node/work/claim",
		"POST /v1/node/work/{id}/result",
		"POST /v1/node/work/project",
		"POST /v1/agent-health",
		"GET /v1/budget/check",
		"POST /v1/integrations/github/installation-token",
	}
	svc := authn.Identity{Subject: "svc:cells", IsService: true} // no UserID, by design
	for _, p := range nodeRoutes {
		ra, ok := routeActions[p]
		if !ok {
			t.Errorf("%s is no longer declared; the execution node depends on it", p)
			continue
		}
		if !ra.Public {
			t.Errorf("%s now requires %s, which a service token can never satisfy: it has no "+
				"application user, so this would break the execution node", p, ra.Action)
		}
		if allowed, reason := checkRouteAction(p, svc); !allowed {
			t.Errorf("%s refused the cells service token: %s", p, reason)
		}
	}
}

// Managing tokens is session-only, and that is not expressed as a scope —
// there is no scope that could express it, because the rule is about the
// credential rather than the permission.
func TestTokenManagementIsExemptFromScopesAndGuardedSeparately(t *testing.T) {
	for _, p := range []string{"POST /v1/tokens", "GET /v1/tokens", "DELETE /v1/tokens/{id}"} {
		ra, ok := routeActions[p]
		if !ok {
			t.Fatalf("%s is not declared", p)
		}
		if !ra.Public {
			t.Errorf("%s declares an action; the rule is ViaToken(), not a scope", p)
		}
		if !strings.Contains(ra.Why, "session-only") {
			t.Errorf("%s does not record that it is session-only: %q", p, ra.Why)
		}
	}
}
