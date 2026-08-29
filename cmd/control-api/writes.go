package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/datopian/workgraph/internal/attention"
	"github.com/datopian/workgraph/internal/authn"
	"github.com/datopian/workgraph/internal/idempotency"
)

// The write surface over domain code that already existed (wg-p4h.4).
//
// internal/attention has had Snooze, Delegate and Resolve since it was written,
// tested, and reachable from nothing. Plan §12.3 asks for them as endpoints.
// Most of this file is exposure rather than construction, which is the whole
// reason this bead was cheap and worth doing before the spec in wg-p4h.5.
//
// Every write here goes through withIdempotency, because the caller is now a
// program that retries.

// maxBody bounds a request. Large enough for a real payload, small enough that
// a mistake is refused rather than buffered.
const maxBody = 64 << 10

// writeContext is what a write handler needs from the request.
type writeContext struct {
	id   authn.Identity
	body []byte
}

// withIdempotency wraps a write handler so a repeat is a replay rather than a
// second effect.
//
// The key is OPTIONAL. Requiring it would break the interface, which is a
// person clicking once and has never sent one — and a mandatory key that
// browsers do not send is a mechanism nobody can adopt incrementally. Absent a
// key the handler simply runs, exactly as before.
//
// The response is recorded only for a SUCCESS. Replaying a failure would pin a
// transient error in place: a caller who retries after a 500 wants another
// attempt, not the same 500 returned faster.
func withIdempotency(
	store *idempotency.Store,
	log *slog.Logger,
	route string,
	fn func(http.ResponseWriter, *http.Request, writeContext),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "could not read the request"})
			return
		}

		key := r.Header.Get(idempotency.Header)
		if key != "" && !idempotency.ValidKey(key) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "the idempotency key is too long",
				"code":  "invalid_idempotency_key",
			})
			return
		}

		digest := idempotency.Digest(body)

		if key != "" && store != nil {
			rec, err := store.Lookup(r.Context(), id.UserID, key, route, digest)
			switch {
			case errors.Is(err, idempotency.ErrConflict):
				// Not a replay. The caller sent a different request under the
				// same key, which is a bug on their side, and hiding it behind
				// the old response would give them the wrong answer.
				writeJSON(w, http.StatusConflict, map[string]any{
					"error": "this idempotency key was already used for a different request",
					"code":  "idempotency_key_conflict",
				})
				return
			case err != nil:
				log.Error("idempotency lookup", "route", route, "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
				return
			case rec != nil:
				log.Info("idempotent replay", "route", route, "user", id.UserID)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Idempotent-Replay", "true")
				w.WriteHeader(rec.StatusCode)
				_, _ = w.Write(rec.Body)
				return
			}
		}

		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		fn(rec, r, writeContext{id: id, body: body})

		if key != "" && store != nil && rec.status >= 200 && rec.status < 300 {
			if err := store.Record(r.Context(), id.UserID, key, route, digest, rec.status, rec.body); err != nil {
				// The write already happened. Failing the response now would
				// invite a retry that repeats it, which is the opposite of what
				// this mechanism is for.
				log.Error("recording an idempotency key", "route", route, "error", err)
			}
		}
	}
}

// recorder captures what a handler wrote so it can be replayed.
type recorder struct {
	http.ResponseWriter
	status int
	body   []byte
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.body = append(r.body, b...)
	return r.ResponseWriter.Write(b)
}

// registerWrites adds the endpoints plan §12.3 asks for over domain code that
// already exists.
func registerWrites(
	authed *http.ServeMux,
	inbox *attention.Store,
	idem *idempotency.Store,
	log *slog.Logger,
) {
	if inbox == nil {
		return
	}

	// Snooze an attention item until a time the caller names.
	authed.HandleFunc("POST /v1/attention/{id}/snooze",
		withIdempotency(idem, log, "POST /v1/attention/{id}/snooze",
			func(w http.ResponseWriter, r *http.Request, wc writeContext) {
				var body struct {
					Until string `json:"until"`
				}
				if err := json.Unmarshal(wc.body, &body); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
					return
				}
				until, err := time.Parse(time.RFC3339, body.Until)
				if err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{
						"error": "until must be an RFC 3339 timestamp",
						"code":  "invalid_until",
					})
					return
				}
				if err := inbox.Snooze(r.Context(), wc.id.UserID, r.PathValue("id"), until); err != nil {
					// The domain refuses a snooze that ends in the past, and
					// row-level security means an item belonging to someone
					// else simply is not there. Both are the caller's problem
					// and neither reveals which.
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"status": "snoozed", "until": until.UTC().Format(time.RFC3339)})
			}))

	// Hand an item to someone else. The item MOVES rather than being copied:
	// two people each believing the other is handling it is the failure
	// delegation exists to remove.
	authed.HandleFunc("POST /v1/attention/{id}/delegate",
		withIdempotency(idem, log, "POST /v1/attention/{id}/delegate",
			func(w http.ResponseWriter, r *http.Request, wc writeContext) {
				var body struct {
					ToUserID string `json:"to_user_id"`
				}
				if err := json.Unmarshal(wc.body, &body); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
					return
				}
				if err := inbox.Delegate(r.Context(), wc.id.UserID, r.PathValue("id"), body.ToUserID); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"status": "delegated", "to": body.ToUserID})
			}))

	// Close an item.
	authed.HandleFunc("POST /v1/attention/{id}/resolve",
		withIdempotency(idem, log, "POST /v1/attention/{id}/resolve",
			func(w http.ResponseWriter, r *http.Request, wc writeContext) {
				if err := inbox.Resolve(r.Context(), wc.id.UserID, r.PathValue("id")); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"status": "resolved"})
			}))
}

// notImplemented answers for a surface plan §12.3 names and no domain code
// backs.
//
// 501 with the owning work package, rather than 404 or a stub that returns an
// empty list. A 404 says "no such endpoint" to a client author reading the
// plan, and an empty list says "nothing here yet" about a feature that does not
// exist — both send them looking for a bug. This says what is true.
func notImplemented(pkg string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "this part of the API is not built yet",
			"code":  "not_implemented",
			"owner": pkg,
		})
	}
}
