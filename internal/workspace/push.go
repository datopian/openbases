package workspace

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// PushMessage is the Pub/Sub envelope, in the fields that matter.
type PushMessage struct {
	Message struct {
		ID         string            `json:"messageId"`
		Data       string            `json:"data"`
		Attributes map[string]string `json:"attributes"`
		Publish    string            `json:"publishTime"`
		// Workspace Events sets the ordering key to the subscription that
		// produced the delivery — the same value as the ce-source attribute,
		// and a second place to find it if the attribute names change again.
		OrderingKey string `json:"orderingKey"`
	} `json:"message"`
	// Subscription is the PUB/SUB subscription, not the Workspace Events one.
	// Two different things with the same word: this names the queue every source
	// shares, so it attributes nothing.
	Subscription string `json:"subscription"`
}

// Receiver handles one Pub/Sub push delivery.
//
// It does exactly two things: prove the delivery is Google's, and write down
// that it happened. Everything that acts on the contents happens elsewhere,
// against a source re-read through the Drive or Meet API (ADR-0026).
//
// That split is not tidiness. Pub/Sub redelivers what is not acknowledged
// inside the deadline, so a handler that did the work inline would be retried
// mid-work — and a verified token proves the delivery came from Pub/Sub, not
// that its contents are true.
type Receiver struct {
	Validator *PushValidator
	DB        *sql.DB
	Log       *slog.Logger
}

// ServeHTTP accepts a push delivery.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	if err := r.Validator.Verify(ctx, req.Header.Get("Authorization")); err != nil {
		// 401 and no body. Pub/Sub retries and then dead-letters, which is the
		// right destination for traffic that cannot be authenticated: it is
		// either an attack or a misconfiguration, and both deserve looking at
		// rather than being silently accepted or silently dropped.
		r.Log.Warn("refused a push delivery", "error", err, "remote", req.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, 4<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var msg PushMessage
	if err := json.Unmarshal(body, &msg); err != nil || msg.Message.ID == "" {
		// A 400, not a 500: retrying will produce the same thing. Without a
		// message id there is no idempotency key, so accepting it would mean
		// processing it again on every redelivery.
		r.Log.Warn("push delivery had no usable message id", "error", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	eventType, target, subscription := describe(msg)

	stored, err := r.record(ctx, msg, eventType, target, subscription, body)
	if err != nil {
		// 500 so Pub/Sub retries. Losing a delivery because our database was
		// briefly unavailable is the one outcome worth a retry storm.
		r.Log.Error("could not record a push delivery", "message", msg.Message.ID, "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if !stored {
		// A repeat. Pub/Sub is at-least-once BY DESIGN, so this is expected
		// rather than exceptional, and acknowledging without reprocessing is
		// the whole reason the receipt is keyed on the message id.
		r.Log.Debug("duplicate push delivery acknowledged", "message", msg.Message.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// record writes the receipt, reporting whether it was new.
func (r *Receiver) record(ctx context.Context, msg PushMessage, eventType, target, subscription string, raw []byte) (bool, error) {
	if r.DB == nil {
		return false, fmt.Errorf("no database")
	}
	var inserted bool
	err := r.DB.QueryRowContext(ctx,
		`SELECT system_record_event($1, $2, $3, $4::jsonb, $5)`,
		msg.Message.ID, nullable(eventType), nullable(target), string(raw),
		nullable(subscription),
	).Scan(&inserted)
	return inserted, err
}

// describe pulls the event type, target and subscription out of a delivery.
//
// The subscription is what attributes the delivery to a source, and it is the
// only one of the three that reliably can. A real delivery looks like this:
//
//	ce-type     google.workspace.drive.file.v3.trashed
//	ce-source   workspaceevents.googleapis.com/subscriptions/drive-file-1E7uX…
//	ce-subject  googleapis.com/drive/v3/files/1c70_FeU54lxUtUWeTDU6BDiARl8AoESU
//
// ce-subject names the FILE. The drive it lives in appears nowhere, so matching
// the target against an allow-listed source's external id can never resolve a
// Drive event — every one was recorded with no source until this was found.
// ce-source names the subscription, and a subscription belongs to exactly one
// source by construction.
//
// Google puts these in message attributes and also inside the base64 data. The
// attributes are read first because they need no decoding; the data is a
// fallback because the attribute names have changed once already and a delivery
// we cannot describe is still one we must record.
func describe(msg PushMessage) (eventType, target, subscription string) {
	eventType = firstOf(msg.Message.Attributes, "ce-type", "eventType", "type")
	target = firstOf(msg.Message.Attributes, "ce-subject", "targetResource", "subject")
	// orderingKey carries the same subscription name, and is present when the
	// attribute is not.
	subscription = firstOf(msg.Message.Attributes, "ce-source", "source")
	if subscription == "" {
		subscription = strings.TrimSpace(msg.Message.OrderingKey)
	}
	if eventType != "" && target != "" && subscription != "" {
		return eventType, target, subscription
	}
	decoded, err := base64.StdEncoding.DecodeString(msg.Message.Data)
	if err != nil {
		return eventType, target, subscription
	}
	var payload struct {
		Subscription struct {
			Name           string `json:"name"`
			TargetResource string `json:"targetResource"`
		} `json:"subscription"`
		EventType string `json:"eventType"`
	}
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return eventType, target, subscription
	}
	if eventType == "" {
		eventType = payload.EventType
	}
	if target == "" {
		target = payload.Subscription.TargetResource
	}
	if subscription == "" {
		subscription = payload.Subscription.Name
	}
	return eventType, target, subscription
}

func firstOf(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(m[k]); v != "" {
			return v
		}
	}
	return ""
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
