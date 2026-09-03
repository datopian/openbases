package domain

import (
	"errors"
	"testing"
)

// What people actually paste. The clipboard contains a URL far more often than
// it contains "owner/name", and refusing the URL would be defensible and
// mildly hostile.
func TestParseFullNameAcceptsWhatPeoplePaste(t *testing.T) {
	for _, in := range []string{
		"datopian/workgraph",
		"  datopian/workgraph  ",
		"https://github.com/datopian/workgraph",
		"https://github.com/datopian/workgraph.git",
		"git@github.com:datopian/workgraph.git",
		"datopian/workgraph/",
	} {
		owner, name, err := ParseFullName(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if owner != "datopian" || name != "workgraph" {
			t.Errorf("%q parsed to %q/%q", in, owner, name)
		}
	}
}

func TestParseFullNameRefusesWhatIsNotARepository(t *testing.T) {
	for _, in := range []string{
		"", "workgraph", "datopian", "/", "datopian/", "/workgraph",
		"datopian/work graph", "a/b/c", "-bad/name", "datopian/.hidden",
	} {
		if _, _, err := ParseFullName(in); err == nil {
			t.Errorf("%q was accepted as a repository", in)
		} else if !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: want ErrInvalid so the handler answers 400, got %v", in, err)
		}
	}
}

// A repository named twice in one call is a mistake upstream, and collapsing it
// quietly hides that. Checked before any database work, so the refusal costs
// nothing and nothing is half-applied.
func TestAttachRefusesADuplicateInTheSameRequest(t *testing.T) {
	s := &Store{}
	_, err := s.AttachRepositories(nil, "u", "p",
		[]string{"datopian/workgraph", "https://github.com/datopian/workgraph"})
	if err == nil {
		t.Fatal("the same repository listed twice should be refused")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("want ErrInvalid, got %v", err)
	}
}

func TestAttachRefusesAnEmptyList(t *testing.T) {
	s := &Store{}
	if _, err := s.AttachRepositories(nil, "u", "p", nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("want ErrInvalid for an empty list, got %v", err)
	}
}

// Parsing happens before the transaction opens, so one bad name in a batch
// refuses the whole call rather than attaching a prefix of it.
func TestOneBadNameRefusesTheBatchBeforeTouchingTheDatabase(t *testing.T) {
	s := &Store{} // nil db: reaching it at all would panic, which is the assertion
	_, err := s.AttachRepositories(nil, "u", "p",
		[]string{"datopian/good", "not-a-repo"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid before any database work, got %v", err)
	}
}
