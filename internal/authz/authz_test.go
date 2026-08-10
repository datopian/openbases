package authz

import (
	"context"
	"errors"
	"testing"
)

func TestAuthorize_DeniesByDefault(t *testing.T) {
	a := NewGrantSet()
	err := a.Authorize(context.Background(), Request{UserID: "u1", Action: ProjectRead, ProjectID: "p1"})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("an empty grant set must deny everything, got %v", err)
	}
}

func TestAuthorize_ProjectGrantDoesNotLeakAcrossProjects(t *testing.T) {
	a := NewGrantSet(Grant{UserID: "u1", Action: ProjectRead, ProjectID: "p1"})
	ctx := context.Background()

	if err := a.Authorize(ctx, Request{UserID: "u1", Action: ProjectRead, ProjectID: "p1"}); err != nil {
		t.Fatalf("granted project must be readable: %v", err)
	}
	if err := a.Authorize(ctx, Request{UserID: "u1", Action: ProjectRead, ProjectID: "p2"}); !errors.Is(err, ErrDenied) {
		t.Fatal("a grant on p1 must not authorise p2 — this is the client isolation property")
	}
	if err := a.Authorize(ctx, Request{UserID: "u2", Action: ProjectRead, ProjectID: "p1"}); !errors.Is(err, ErrDenied) {
		t.Fatal("a grant to u1 must not authorise u2")
	}
}

func TestAuthorize_UnknownActionIsDenied(t *testing.T) {
	a := NewGrantSet(Grant{UserID: "u1", Action: "project.reed"})
	err := a.Authorize(context.Background(), Request{UserID: "u1", Action: "project.reed"})
	if !errors.Is(err, ErrDenied) {
		t.Fatal("a misspelled or unregistered action must be denied, never allowed")
	}
}

func TestAuthorize_AnonymousIsDenied(t *testing.T) {
	a := NewGrantSet(Grant{UserID: "", Action: ProjectRead})
	if err := a.Authorize(context.Background(), Request{Action: ProjectRead}); !errors.Is(err, ErrDenied) {
		t.Fatal("an empty user ID must never authorise anything")
	}
}

func TestProtectedActions(t *testing.T) {
	protected := []Action{PullRequestMerge, DeploymentExecute, SecretManage, PolicyManage,
		MarketingPublish, KnowledgeClassificationDowngrade}
	for _, a := range protected {
		if !a.Protected() {
			t.Errorf("%s must be treated as a protected action", a)
		}
	}
	for _, a := range []Action{ProjectRead, WorkCreate, AgentInspect} {
		if a.Protected() {
			t.Errorf("%s should not require approval for every use", a)
		}
	}
}
