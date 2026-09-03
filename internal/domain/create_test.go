package domain

import (
	"errors"
	"strings"
	"testing"
)

// Validate is where a person's typing meets the schema's constraints. Every
// case here is one a constraint would also have caught, and the point is the
// message: "projects_visibility_check" is accurate and tells nobody what to
// type instead.
func TestValidateRefusesWhatTheSchemaWouldRefuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   NewProject
		want string
	}{
		{"no slug", NewProject{Name: "X", BackupOwner: "b@x.com"}, "a slug is required"},
		{"no name", NewProject{Slug: "x", BackupOwner: "b@x.com"}, "a name is required"},
		{"no backup owner", NewProject{Slug: "x", Name: "X"}, "a backup owner is required"},
		{"slug with a space", NewProject{Slug: "my project", Name: "X", BackupOwner: "b@x.com"},
			"lower-case letters, digits and single hyphens"},
		{"slug with an underscore", NewProject{Slug: "my_project", Name: "X", BackupOwner: "b@x.com"},
			"will not do"},
		{"slug with a double hyphen", NewProject{Slug: "a--b", Name: "X", BackupOwner: "b@x.com"},
			"will not do"},
		{"slug ending in a hyphen", NewProject{Slug: "ab-", Name: "X", BackupOwner: "b@x.com"},
			"will not do"},
		{"unknown visibility", NewProject{Slug: "x", Name: "X", BackupOwner: "b@x.com", Visibility: "secret"},
			"internal, confidential or restricted"},
		{"restricted without a cell", NewProject{Slug: "x", Name: "X", BackupOwner: "b@x.com", Visibility: "restricted"},
			"runs in its own execution cell"},
		{"one person owning both ends", NewProject{Slug: "x", Name: "X",
			PrimaryOwner: "a@x.com", BackupOwner: "a@x.com"}, "must differ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if err == nil {
				t.Fatalf("accepted %+v, which the schema would have refused", tc.in)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("want ErrInvalid so the handler can answer 400, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message should say %q, got %q", tc.want, err)
			}
		})
	}
}

func TestValidateAcceptsTheQuickCase(t *testing.T) {
	// The whole point of defaulting the primary owner to the caller: a slug, a
	// name and one other person is enough to create a project.
	n := NewProject{Slug: "roseville-poc", Name: "Roseville PoC", BackupOwner: "b@x.com"}
	if err := n.Validate(); err != nil {
		t.Fatalf("the minimal case should be valid: %v", err)
	}
	if n.Visibility != "internal" {
		t.Errorf("visibility should default to internal, got %q", n.Visibility)
	}
}

func TestValidateNormalisesBeforeChecking(t *testing.T) {
	n := NewProject{Slug: "  Roseville-POC ", Name: "  Roseville  ",
		BackupOwner: " B@X.COM ", Visibility: " INTERNAL "}
	if err := n.Validate(); err != nil {
		t.Fatalf("should normalise rather than refuse: %v", err)
	}
	if n.Slug != "roseville-poc" {
		t.Errorf("slug should be trimmed and lower-cased, got %q", n.Slug)
	}
	if n.BackupOwner != "b@x.com" {
		t.Errorf("an address is matched lower-cased, got %q", n.BackupOwner)
	}
	if n.Name != "Roseville" {
		t.Errorf("name should be trimmed but not lower-cased, got %q", n.Name)
	}
}

// A slug reaches URLs, branch names, unit names and Beads prefixes. Anything
// that survives Validate has to be safe in all of them.
func TestAnAcceptedSlugIsSafeEverywhereItIsUsed(t *testing.T) {
	for _, s := range []string{"a", "ab", "a-b", "a-b-c", "poc2", "roseville-poc"} {
		n := NewProject{Slug: s, Name: "X", BackupOwner: "b@x.com"}
		if err := n.Validate(); err != nil {
			t.Errorf("%q should be a usable slug: %v", s, err)
		}
	}
	// Case is NOT in this list: Validate lower-cases before checking, so
	// "Roseville-POC" is normalised rather than refused. That is deliberate and
	// TestValidateNormalisesBeforeChecking pins it.
	for _, s := range []string{"", "-a", "a-", "a--b", "a b", "a_b", "a/b", "a.b",
		"../etc", "a%20b", "a\nb"} {
		n := NewProject{Slug: s, Name: "X", BackupOwner: "b@x.com"}
		if err := n.Validate(); err == nil {
			t.Errorf("%q was accepted as a slug and it reaches URLs and unit names", s)
		}
	}
}
