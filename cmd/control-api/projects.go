package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/datopian/workgraph/internal/authn"
	"github.com/datopian/workgraph/internal/domain"
	"github.com/datopian/workgraph/internal/idempotency"
)

// Creating a project, and attaching repositories to one (wg-1dm, wg-m6p).
//
// Both tables were reachable only by writing SQL on the node. That is the whole
// of "I could not create a project": nothing was hidden by row-level security —
// an organisation_admin can already see every project, every repository and
// every bead — there was simply no way to make one.

// projectErrStatus maps a domain error to the status and code a caller can act
// on.
//
// Written once because four handlers need the same mapping, and because getting
// it wrong produces exactly the dead end wg-bof fixed: a refusal the caller
// caused, reported as an internal error they cannot do anything about.
func projectErrStatus(err error) (int, map[string]any) {
	switch {
	case errors.Is(err, domain.ErrInvalid):
		return http.StatusBadRequest, map[string]any{"error": err.Error(), "code": "invalid_project"}
	case errors.Is(err, domain.ErrConflict):
		return http.StatusConflict, map[string]any{"error": err.Error(), "code": "slug_taken"}
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, map[string]any{"error": "no such project", "code": "not_found"}
	default:
		return 0, nil
	}
}

// registerProjectWrites adds project creation and repository attachment.
func registerProjectWrites(
	authed *http.ServeMux,
	store *domain.Store,
	idem *idempotency.Store,
	log *slog.Logger,
) {
	if store == nil {
		return
	}

	authed.HandleFunc("POST /v1/projects",
		withIdempotency(idem, log, "POST /v1/projects",
			func(w http.ResponseWriter, r *http.Request, wc writeContext) {
				var n domain.NewProject
				if err := json.Unmarshal(wc.body, &n); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
					return
				}
				p, err := store.CreateProject(r.Context(), wc.id.UserID, n)
				if err != nil {
					if status, body := projectErrStatus(err); status != 0 {
						writeJSON(w, status, body)
						return
					}
					log.Error("creating a project", "error", err, "slug", n.Slug)
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
					return
				}
				log.Info("project created", "slug", p.Slug, "by", wc.id.UserID)
				writeJSON(w, http.StatusCreated, map[string]any{"project": p})
			}))

	authed.HandleFunc("GET /v1/projects/{slug}/repositories", func(w http.ResponseWriter, r *http.Request) {
		ident, _ := authn.FromContext(r.Context())
		id := ident.UserID
		if id == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		repos, err := store.ListRepositories(r.Context(), id, r.PathValue("slug"))
		if err != nil {
			if status, body := projectErrStatus(err); status != 0 {
				writeJSON(w, status, body)
				return
			}
			log.Error("listing repositories", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"repositories": repos})
	})

	// Attaching several at once, because onboarding a project with six
	// repositories should not be six round trips, and because a partial result
	// has to be reportable: one repository already attached elsewhere must not
	// discard the others.
	authed.HandleFunc("POST /v1/projects/{slug}/repositories",
		withIdempotency(idem, log, "POST /v1/projects/{slug}/repositories",
			func(w http.ResponseWriter, r *http.Request, wc writeContext) {
				var body struct {
					Repositories []string `json:"repositories"`
				}
				if err := json.Unmarshal(wc.body, &body); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
					return
				}
				results, err := store.AttachRepositories(r.Context(), wc.id.UserID,
					r.PathValue("slug"), body.Repositories)
				if err != nil {
					if status, b := projectErrStatus(err); status != 0 {
						writeJSON(w, status, b)
						return
					}
					log.Error("attaching repositories", "error", err)
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
					return
				}
				// 200 rather than 201 even when something was created: the
				// response is a per-repository report, and some of those
				// repositories may have been refused. A 201 would claim more
				// than happened.
				writeJSON(w, http.StatusOK, map[string]any{"results": results})
			}))

	authed.HandleFunc("DELETE /v1/projects/{slug}/repositories/{owner}/{name}",
		withIdempotency(idem, log, "DELETE /v1/projects/{slug}/repositories/{owner}/{name}",
			func(w http.ResponseWriter, r *http.Request, wc writeContext) {
				err := store.DetachRepository(r.Context(), wc.id.UserID,
					r.PathValue("slug"), r.PathValue("owner"), r.PathValue("name"))
				if err != nil {
					if status, b := projectErrStatus(err); status != 0 {
						writeJSON(w, status, b)
						return
					}
					log.Error("detaching a repository", "error", err)
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"status": "detached"})
			}))
}

// beadLabels renders a bead's labels as a PostgreSQL text[] literal.
//
// NULL rather than '{}' for an empty list, because system_project_bead
// distinguishes the two: NULL means this caller does not report labels at all,
// which is what an older dispatcher looks like, and an empty array means the
// bead genuinely has none. Both fall through to the cell rule, so the
// distinction changes no behaviour today — it exists so that adding a rule for
// "labelled with nothing" later does not silently also apply to callers that
// never spoke about labels.
func beadLabels(vs []string) any {
	if len(vs) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, v := range vs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}
