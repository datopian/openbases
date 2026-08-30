package workspace

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func envelope(id, ceType, subject string) string {
	m := PushMessage{}
	m.Message.ID = id
	m.Message.Attributes = map[string]string{"ce-type": ceType, "ce-subject": subject}
	m.Message.Publish = now.Format(time.RFC3339)
	b, _ := json.Marshal(m)
	return string(b)
}

// A delivery that cannot be authenticated is refused before anything is read.
// The Access bypass is a routing decision; this is the boundary (ADR-0026).
func TestAnUnauthenticatedDeliveryIsRefusedAndNothingIsRead(t *testing.T) {
	h := newHarness(t)
	r := &Receiver{Validator: h.v, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	req := httptest.NewRequest(http.MethodPost, "/v1/google/events",
		strings.NewReader(envelope("m1", "created", "//drive.googleapis.com/drives/x")))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	// DB is nil: if the handler had tried to record before verifying, this
	// would have panicked or 500'd instead.
	if w.Body.Len() != 0 {
		t.Errorf("a refused delivery returned a body: %q", w.Body.String())
	}
}

// Without Pub/Sub's message id there is no idempotency key, so the delivery
// would be reprocessed on every redelivery. A 400, because retrying produces
// the same thing.
func TestADeliveryWithNoMessageIdIsRefusedWithoutRetry(t *testing.T) {
	h := newHarness(t)
	r := &Receiver{Validator: h.v, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := httptest.NewRequest(http.MethodPost, "/v1/google/events",
		strings.NewReader(`{"message":{"data":"e30="}}`))
	req.Header.Set("Authorization", "Bearer "+h.token(t, nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — a 500 would make Pub/Sub retry something that cannot succeed", w.Code)
	}
}

// Google has moved these attribute names once already, so the decoder falls
// back to the payload rather than recording a delivery it cannot describe.
func TestTheEventIsDescribedFromAttributesOrFromTheBody(t *testing.T) {
	et, target := describe(mustEnvelope(t, envelope("m", "google.workspace.drive.file.v3.created",
		"//drive.googleapis.com/drives/0AC")))
	if et != "google.workspace.drive.file.v3.created" || !strings.Contains(target, "0AC") {
		t.Errorf("attributes not read: %q %q", et, target)
	}

	// Attributes absent: fall back to the base64 data.
	inner, _ := json.Marshal(map[string]any{
		"eventType":    "google.workspace.drive.file.v3.trashed",
		"subscription": map[string]string{"targetResource": "//drive.googleapis.com/drives/0AD"},
	})
	var m PushMessage
	m.Message.ID = "m2"
	m.Message.Data = base64.StdEncoding.EncodeToString(inner)
	et, target = describe(m)
	if et != "google.workspace.drive.file.v3.trashed" || !strings.Contains(target, "0AD") {
		t.Errorf("payload fallback not used: %q %q", et, target)
	}

	// Undescribable, but still recordable: describe must not panic or invent.
	var empty PushMessage
	empty.Message.ID = "m3"
	empty.Message.Data = "not-base64!!"
	if et, target = describe(empty); et != "" || target != "" {
		t.Errorf("described an undescribable delivery: %q %q", et, target)
	}
}

func mustEnvelope(t *testing.T, s string) PushMessage {
	t.Helper()
	var m PushMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// An unrecognised Google state must not be read as active. The safe direction
// is to replace something we do not understand, not to leave it running.
func TestAnUnknownGoogleStateIsNotTreatedAsHealthy(t *testing.T) {
	for _, s := range []string{"", "PENDING", "STATE_UNSPECIFIED", "something-new"} {
		if got := (GoogleSubscription{State: s}).LifecycleState(); got == StateActive {
			t.Errorf("state %q was read as active", s)
		}
	}
	if got := (GoogleSubscription{State: "ACTIVE"}).LifecycleState(); got != StateActive {
		t.Errorf("ACTIVE was read as %q", got)
	}
	if got := (GoogleSubscription{State: "SUSPENDED"}).LifecycleState(); got != StateSuspended {
		t.Errorf("SUSPENDED was read as %q", got)
	}
}

func TestExpiryIsParsedOrReportedMissing(t *testing.T) {
	if _, ok := (GoogleSubscription{}).Expiry(); ok {
		t.Error("a subscription with no expireTime reported an expiry")
	}
	if _, ok := (GoogleSubscription{ExpireTime: "not a time"}).Expiry(); ok {
		t.Error("an unparseable expireTime reported an expiry")
	}
	got, ok := (GoogleSubscription{ExpireTime: "2026-09-05T12:00:00Z"}).Expiry()
	if !ok || !got.Equal(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("expiry = %v (%v)", got, ok)
	}
}

// The caller's decision differs by status, so the error has to carry it: a 404
// means recreate, a 403 means the delegation is wrong and retrying never helps.
func TestAPIErrorsDistinguishGoneFromDenied(t *testing.T) {
	if !(&APIError{Status: 404}).NotFound() {
		t.Error("404 is not reported as not-found")
	}
	for _, s := range []int{401, 403} {
		if !(&APIError{Status: s}).Denied() {
			t.Errorf("%d is not reported as denied", s)
		}
	}
	if (&APIError{Status: 500}).Denied() || (&APIError{Status: 500}).NotFound() {
		t.Error("500 was classified as denied or not-found; it is a retry")
	}
}
