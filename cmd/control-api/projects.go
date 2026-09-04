package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/datopian/workgraph/internal/authn"
	"github.com/datopian/workgraph/internal/authz"
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

// registerPlatformState adds the read-only view of what the platform is doing
// (wg-7bh).
//
// Every number here already existed in the database and nothing could reach it:
// three cells, twenty thousand agent health events, thirteen open alerts and
// seventy-five unattributed beads, all visible only from a database shell on the
// control node. An answer reachable only by the person with psql is the same
// failure as an alert nobody receives.
func registerPlatformState(authed *http.ServeMux, db *sql.DB, log *slog.Logger) {
	if db == nil {
		return
	}
	authed.HandleFunc("GET /v1/platform", func(w http.ResponseWriter, r *http.Request) {
		ident, _ := authn.FromContext(r.Context())
		if ident.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		// Inside authz.WithUser because system_platform_state is SECURITY
		// DEFINER and gates on is_company_manager(), which reads
		// current_app_user(). Querying db directly sets no identity, the gate
		// sees nobody, and the endpoint would refuse an administrator — which
		// is exactly how GET /v1/work returned an empty list to everyone for a
		// day (wg-ue8).
		var doc []byte
		err := authz.WithUser(r.Context(), db, ident.UserID, func(tx *sql.Tx) error {
			return tx.QueryRowContext(r.Context(), `SELECT system_platform_state()`).Scan(&doc)
		})
		if err != nil {
			// The function raises for a caller who is not company management,
			// which is a 403 rather than a fault. Named as such: the refusal
			// is about the caller's role, and telling them so is the only way
			// they can ask somebody for it.
			if strings.Contains(err.Error(), "company management only") {
				writeJSON(w, http.StatusForbidden, map[string]any{
					"error": "platform state is company management only",
					"code":  "role_grant_missing"})
				return
			}
			log.Error("reading platform state", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})
}

// registerBeadDetail adds the read of one bead: what happened to it, what the
// agent said, and what it cost (wg-m07).
//
// The question this answers is "I dispatched that — did anything happen?", and
// before this the honest answer was to read a JSONL file on the execution node.
func registerBeadDetail(authed *http.ServeMux, db *sql.DB, log *slog.Logger) {
	if db == nil {
		return
	}
	authed.HandleFunc("GET /v1/work/{bead}", func(w http.ResponseWriter, r *http.Request) {
		ident, _ := authn.FromContext(r.Context())
		if ident.UserID == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "no application user"})
			return
		}
		bead := strings.TrimSpace(r.PathValue("bead"))
		if bead == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a bead id is required"})
			return
		}

		// Inside authz.WithUser: system_bead_detail is SECURITY DEFINER and
		// checks can_read_project against current_app_user(), so a query
		// issued without an identity would be answered as nobody.
		var doc []byte
		err := authz.WithUser(r.Context(), db, ident.UserID, func(tx *sql.Tx) error {
			return tx.QueryRowContext(r.Context(),
				`SELECT system_bead_detail($1)`, bead).Scan(&doc)
		})
		if err != nil {
			log.Error("reading a bead", "bead", bead, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		if len(doc) == 0 || string(doc) == "null" {
			// A bead in a project the caller may not see is answered exactly
			// like a bead that does not exist. Confirming its existence would
			// leak the project (ADR-0013).
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": "no such bead, or none you can see",
				"code":  "not_found",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})
}

// rigForBead picks a rig in the cell that holds one of the bead's project's
// repositories (wg-ugb).
//
// Three answers, and the middle one is the point:
//
//	rig, "", nil   route here
//	"",  why, nil  refuse, and `why` says what is missing
//	"",  "",  nil   the bead has no project, so there is nothing to route on and
//	                the dispatcher's default stands — company-wide work has no
//	                repository to match, and refusing it would break the case
//	                that worked before this existed.
func rigForBead(ctx context.Context, db *sql.DB, bead, cell string) (rig, why string, err error) {
	// Read outside authz.WithUser deliberately: this is the dispatch path's own
	// routing decision, not a read on the caller's behalf, and the caller's
	// permission to dispatch this bead has already been settled above. The
	// function it calls is SECURITY DEFINER and joins only rig and repository
	// names, which are not secrets.
	rows, qerr := db.QueryContext(ctx,
		`SELECT rig, repository FROM system_rigs_for_bead($1, $2)`, bead, cell)
	if qerr != nil {
		return "", "", qerr
	}
	defer rows.Close()

	type candidate struct{ rig, repo string }
	var found []candidate
	for rows.Next() {
		var c candidate
		if scanErr := rows.Scan(&c.rig, &c.repo); scanErr != nil {
			return "", "", scanErr
		}
		found = append(found, c)
	}
	if rerr := rows.Err(); rerr != nil {
		return "", "", rerr
	}

	if len(found) == 1 {
		return found[0].rig, "", nil
	}
	if len(found) > 1 {
		// Several rigs could do it, so the choice is the caller's rather than
		// ours. Picking one would be arbitrary and would hide the ambiguity
		// until somebody wondered why their work ran in the wrong checkout.
		names := make([]string, 0, len(found))
		for _, c := range found {
			names = append(names, c.rig+" ("+c.repo+")")
		}
		return "", "several rigs in this cell hold a repository for this bead's project, " +
			"so name one: " + strings.Join(names, ", "), nil
	}

	// None matched. Distinguish the two reasons, because they have different
	// fixes: a project with no registered repository needs one attaching, and a
	// project whose repositories no rig holds needs a rig.
	var project string
	var repos int
	if qerr := db.QueryRowContext(ctx, `
		SELECT COALESCE(p.slug, ''),
		       (SELECT count(*) FROM project_repositories r WHERE r.project_id = p.id)
		  FROM work_refs w
		  JOIN projects p ON p.id = w.project_id
		 WHERE w.bead_id = $1
		 LIMIT 1`, bead).Scan(&project, &repos); qerr != nil {
		if errors.Is(qerr, sql.ErrNoRows) {
			// No project: nothing to route on, and not an error. The
			// dispatcher's default rig stands, which is what happened before
			// this check existed.
			return "", "", nil
		}
		return "", "", qerr
	}

	if repos == 0 {
		return "", "the project " + project + " has no repository registered, so there is " +
			"nowhere to run this bead. Attach one with POST /v1/projects/" + project +
			"/repositories.", nil
	}
	return "", "no rig in cell " + cell + " holds a repository belonging to " + project +
		". The work would otherwise run in a disposable sandbox and report success " +
		"without doing anything.", nil
}
