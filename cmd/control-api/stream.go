package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/datopian/openbases/internal/authn"
	"github.com/datopian/openbases/internal/events"
)

// The event stream and cursor pagination (wg-p4h.8).
//
// Without these an agent that wants to know when a dispatch finished polls
// /v1/work every few seconds, per agent, forever — a cost with no ceiling that
// arrives as database load on the control node rather than as a bill anybody
// notices.
//
// Server-sent events rather than WebSockets: the traffic is one-directional, SSE
// survives a proxy that buffers, and Last-Event-ID is a resume protocol clients
// already implement. A WebSocket would be a second connection type to operate
// for no property this needs.

// streamPoll is how often the stream looks for new events.
//
// The origin still polls the database — but ONE query per connected client per
// second replaces every client polling /v1/work independently, and this query is
// an indexed read of a single append-only table by primary key rather than the
// projection /v1/work builds. LISTEN/NOTIFY would remove even that; it is not
// done here because it needs a dedicated connection per listener and the win
// does not justify that until there are more clients than there are today.
const streamPoll = time.Second

// streamMaxAge bounds a connection. A client that reconnects gets a fresh
// database session and a fresh RLS context, which is what makes a revoked
// grant take effect on a stream rather than persisting for as long as the
// connection does.
const streamMaxAge = 30 * time.Minute

func registerStream(authed *http.ServeMux, store *events.Store, log *slog.Logger) {
	if store == nil {
		return
	}

	// Cursor-paginated reads, for a client catching up rather than following.
	authed.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		after, err := cursorOf(r.URL.Query().Get("after"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "after must be an event id", "code": "invalid_cursor",
			})
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

		page, err := store.After(r.Context(), id.UserID, after, limit)
		if err != nil {
			log.Error("reading events", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		if page.Events == nil {
			page.Events = []events.Event{}
		}
		writeJSON(w, http.StatusOK, page)
	})

	// The stream.
	authed.HandleFunc("GET /v1/events/stream", func(w http.ResponseWriter, r *http.Request) {
		id, _ := authn.FromContext(r.Context())
		if id.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unavailable"})
			return
		}

		// Resume where the client left off. Last-Event-ID is the header the
		// browser EventSource sends automatically on reconnect; the query
		// parameter is for everything else.
		after, err := cursorOf(firstNonEmpty(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after")))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "Last-Event-ID must be an event id", "code": "invalid_cursor",
			})
			return
		}

		// With no cursor, start from now. Replaying the whole log to a client
		// that only wants to follow would be a surprising amount of traffic and
		// a surprising amount of history.
		if after == 0 && r.Header.Get("Last-Event-ID") == "" && r.URL.Query().Get("after") == "" {
			if latest, err := store.Latest(r.Context(), id.UserID); err == nil {
				after = latest
			}
		}

		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-store")
		h.Set("Connection", "keep-alive")
		// Proxies that buffer turn a stream into a very slow batch.
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ctx := r.Context()
		deadline := time.NewTimer(streamMaxAge)
		defer deadline.Stop()
		tick := time.NewTicker(streamPoll)
		defer tick.Stop()

		log.Info("event stream opened", "user", id.UserID, "token", id.TokenID, "from", after)
		defer log.Info("event stream closed", "user", id.UserID, "token", id.TokenID)

		for {
			select {
			case <-ctx.Done():
				return
			case <-deadline.C:
				// Tell the client this was expected, so it reconnects rather
				// than reporting an error to whoever is watching.
				fmt.Fprintf(w, "event: reconnect\ndata: {\"reason\":\"max connection age\"}\n\n")
				flusher.Flush()
				return
			case <-tick.C:
				page, err := store.After(ctx, id.UserID, after, events.DefaultLimit)
				if err != nil {
					log.Error("streaming events", "error", err, "user", id.UserID)
					return
				}
				if len(page.Events) == 0 {
					// A comment line keeps the connection alive through
					// idle-timeout proxies without being an event.
					fmt.Fprint(w, ": keep-alive\n\n")
					flusher.Flush()
					continue
				}
				for _, ev := range page.Events {
					body, err := json.Marshal(ev)
					if err != nil {
						continue
					}
					// id: is what the client echoes back as Last-Event-ID, so
					// this line is the whole resume protocol.
					fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, body)
					after = ev.ID
				}
				flusher.Flush()
			}
		}
	})
}

func cursorOf(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
