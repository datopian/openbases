package events

import (
	"encoding/json"
	"testing"
	"time"
)

// The cursor is an event id, not an offset, and the difference is the whole
// reason this is safe on an append-only log: an offset shifts under inserts, so
// a client paging with OFFSET silently skips rows exactly when the log is
// busiest — which is when somebody is most likely to be reading it.
func TestPageSerialisesWithACursorAClientCanEcho(t *testing.T) {
	p := Page{
		Events: []Event{{
			ID:         42,
			OccurredAt: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC),
			Type:       "agent.completed",
			Payload:    json.RawMessage(`{"bead":"wg-abc"}`),
		}},
		NextCursor: "42",
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back Page
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.NextCursor != "42" || len(back.Events) != 1 || back.Events[0].ID != 42 {
		t.Fatalf("cursor or events did not survive the round trip: %s", b)
	}
	// The payload is passed through, not re-encoded. Re-encoding would reorder
	// keys and change a payload a client may be comparing.
	if string(back.Events[0].Payload) != `{"bead":"wg-abc"}` {
		t.Errorf("payload was rewritten: %s", back.Events[0].Payload)
	}
}

// An empty page must not carry a cursor. Returning one would tell a client
// there is more when there is not, and it would poll for it — the exact
// behaviour this endpoint exists to remove.
func TestAnEmptyPageHasNoCursor(t *testing.T) {
	b, err := json.Marshal(Page{Events: []Event{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"events":[]}` {
		t.Fatalf("an empty page serialised as %s; next_cursor must be omitted", b)
	}
}

// A project the caller cannot see is omitted rather than nulled, so a client
// cannot infer that an event exists in a project it has no access to.
func TestAProjectlessEventOmitsTheField(t *testing.T) {
	b, err := json.Marshal(Event{ID: 1, Type: "backup.completed", Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "" || contains(string(b), `"project"`) {
		t.Fatalf("project should be omitted when absent: %s", b)
	}
}

func TestLimitsAreBounded(t *testing.T) {
	if DefaultLimit <= 0 || DefaultLimit > MaxLimit {
		t.Fatalf("DefaultLimit %d is not a sane default under MaxLimit %d", DefaultLimit, MaxLimit)
	}
	if MaxLimit > 1000 {
		t.Errorf("MaxLimit %d lets a client ask for a copy of the log rather than a page", MaxLimit)
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
