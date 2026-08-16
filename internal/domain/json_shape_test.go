package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

// A list endpoint must serialise as [] and never as null.
//
// A nil slice in Go marshals to `null`, and the browser then calls .map on it
// and throws "x.map is not a function" — which unmounts the whole page and
// leaves a blank screen. This is what broke the interface on its first real
// load, and it only happens when the list is empty, which is exactly the case a
// local test with seeded data never reaches.
func TestEmptyProjectListSerialisesAsArray(t *testing.T) {
	// EmptyProjectList is what the store returns when row-level security filters
	// everything out. Asserting against a bare nil slice would only be testing
	// that Go marshals nil as null, which it does and always will.
	projects := EmptyProjectList()

	b, err := json.Marshal(projects)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "null" {
		t.Fatal("an empty project list serialises as null; the browser will call .map on it and crash")
	}
	if string(b) != "[]" {
		t.Fatalf("expected [], got %s", b)
	}
}

// The detail payload has two fields competing for the same JSON name:
// ProjectSummary.Repositories is an int count, and ProjectDetail.Repositories
// is the array. Go resolves the conflict by depth, so the array must win — if
// the count won, the page would call .map on a number.
func TestDetailRepositoriesIsTheArrayNotTheCount(t *testing.T) {
	d := ProjectDetail{
		ProjectSummary: ProjectSummary{Slug: "p", Repositories: 7},
		Repositories:   []RepositoryStatus{{FullName: "a/b"}},
	}

	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["repositories"].([]any); !ok {
		t.Fatalf("repositories is %T, not an array; the page would call .map on it: %s", got["repositories"], b)
	}
}

// Every array-valued field in the detail payload must be an array when empty.
func TestDetailArraysAreNeverNull(t *testing.T) {
	// Constructed with nil slices on purpose: the type must normalise them.
	b, err := json.Marshal(ProjectDetail{ProjectSummary: ProjectSummary{Slug: "empty"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"repositories", "signals"} {
		if strings.Contains(string(b), `"`+field+`":null`) {
			t.Errorf("%s serialises as null when empty; the page calls .map on it", field)
		}
	}
}
