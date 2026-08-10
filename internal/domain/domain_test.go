package domain

import "testing"

func TestInheritVisibility_TakesMostRestrictive(t *testing.T) {
	got, err := InheritVisibility(VisibilityInternal, VisibilityRestricted, VisibilityConfidential)
	if err != nil {
		t.Fatal(err)
	}
	if got != VisibilityRestricted {
		t.Errorf("got %q, want restricted", got)
	}
}

func TestInheritVisibility_RejectsNoSources(t *testing.T) {
	if _, err := InheritVisibility(); err == nil {
		t.Fatal("an object with no provenance must not receive a default classification")
	}
}

func TestInheritVisibility_RejectsUnknownLevel(t *testing.T) {
	if _, err := InheritVisibility(VisibilityInternal, "public"); err == nil {
		t.Fatal(`"public" is not a recognised classification and must be rejected`)
	}
}

func TestWorkRef_RequiresFullTuple(t *testing.T) {
	if err := (WorkRef{BeadID: "wg-1"}).Validate(); err == nil {
		t.Fatal("a bead ID alone is not a valid work reference")
	}
	full := WorkRef{OrganisationID: "o", ExecutionCellID: "c", BeadsDatabaseID: "d", BeadID: "wg-1"}
	if err := full.Validate(); err != nil {
		t.Fatalf("complete tuple must validate: %v", err)
	}
	if full.String() != "o/c/d/wg-1" {
		t.Errorf("unexpected rendering %q", full.String())
	}
}

func TestProject_Validate(t *testing.T) {
	cases := []struct {
		name    string
		p       Project
		wantErr bool
	}{
		{"valid", Project{Slug: "a", Visibility: VisibilityInternal, PrimaryOwner: "anu", BackupOwner: "eng"}, false},
		{"no backup owner", Project{Slug: "a", Visibility: VisibilityInternal, PrimaryOwner: "anu"}, true},
		{"backup equals primary", Project{Slug: "a", Visibility: VisibilityInternal, PrimaryOwner: "anu", BackupOwner: "anu"}, true},
		{"restricted without cell", Project{Slug: "a", Visibility: VisibilityRestricted, PrimaryOwner: "anu", BackupOwner: "eng"}, true},
		{"restricted with cell", Project{Slug: "a", Visibility: VisibilityRestricted, PrimaryOwner: "anu", BackupOwner: "eng", ExecutionCellID: "cell-1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("wantErr=%v, got %v", tc.wantErr, err)
			}
		})
	}
}
