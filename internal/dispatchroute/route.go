// Package dispatchroute decides which rig runs a bead.
//
// It lives here, rather than in the control API where it was written, because
// there is more than one way to dispatch: the HTTP handler the interface and
// MCP use, and wg-work on the control node, which exists so that "a demo or an
// incident should not depend on a browser". wg-work hardcoded the rig as
// `sandbox` -- the exact failure this routing was written to prevent, still
// reachable from the one tool somebody reaches for when something is wrong.
//
// One copy, so that a second caller cannot quietly disagree with the first.
package dispatchroute

import (
	"context"
	"database/sql"
	"strings"
)

// The codes a refusal can carry.
//
// Separate codes because they need different actions, and a client that can
// only read prose cannot branch. Both used to be no_rig_for_project, which was
// wrong the moment the not-projected case existed: a bead id that does not
// exist is not a rig problem, and telling somebody to provision a rig for a
// typo sends them to fix the wrong thing.
const (
	// CodeNoRig: the bead is real and no rig can run it. Fixed by attaching a
	// repository or provisioning a rig, or by naming one of several.
	CodeNoRig = "no_rig_for_project"
	// CodeNotProjected: no cell has this bead. Fixed by correcting the id, or
	// by waiting for the next projection pass.
	CodeNotProjected = "bead_not_projected"
)

// Refusal is why a bead cannot be dispatched, in terms a client can branch on
// and a person can read.
type Refusal struct {
	Code string
	Why  string
}

func (r *Refusal) Error() string { return r.Why }

// candidate is one rig that could run a bead, and the repository that makes it
// able to.
type candidate struct{ rig, repo string }

// Choose decides between the rigs that can run a bead.
//
// Separated from the query so it can be tested without a database: it holds the
// branch that decides whether a caller-named rig is honoured, which is the one
// place where naming a rig could become a way past the check rather than a
// choice within it. `decided` is false only when nothing matched, which leaves
// the caller to explain WHY nothing matched -- a question that needs the
// database again.
func Choose(found []candidate, wanted string) (rig string, refusal *Refusal, decided bool) {
	names := func() string {
		out := make([]string, 0, len(found))
		for _, c := range found {
			out = append(out, c.rig+" ("+c.repo+")")
		}
		return strings.Join(out, ", ")
	}

	// A named rig has to be one of them. This is the case the PortalJS project
	// reaches every time: it holds datopian/portaljs AND
	// datopian/cloud.portaljs.com, so two rigs can do the work and the caller
	// must say which -- but saying `sandbox` is not an answer to that question.
	// The first version of the handler skipped the whole check when a rig was
	// named, which reproduced exactly the failure the check exists to prevent.
	if w := strings.TrimSpace(wanted); w != "" && len(found) > 0 {
		for _, c := range found {
			if c.rig == w {
				return c.rig, nil, true
			}
		}
		return "", &Refusal{Code: CodeNoRig,
			Why: "no rig called " + w + " in this cell holds a repository for this " +
				"bead's project; the ones that do are: " + names()}, true
	}

	if len(found) == 1 {
		return found[0].rig, nil, true
	}
	if len(found) > 1 {
		// Several rigs could do it, so the choice is the caller's rather than
		// ours. Picking one would be arbitrary and would hide the ambiguity
		// until somebody wondered why their work ran in the wrong checkout.
		return "", &Refusal{Code: CodeNoRig,
			Why: "several rigs in this cell hold a repository for this bead's project, " +
				"so name one: " + names()}, true
	}
	return "", nil, false
}

// For is the rig that should run a bead, or the reason none can.
//
// wanted is the rig the caller asked for, or empty to let the registry choose.
// A caller who names one is choosing among the rigs that can do the work; it is
// not a way past the check.
func For(ctx context.Context, db *sql.DB, bead, cell, wanted string) (rig string, refusal *Refusal, err error) {
	// Read outside authz.WithUser deliberately: this is the dispatch path's own
	// routing decision, not a read on the caller's behalf, and the caller's
	// permission to dispatch this bead has already been settled above. The
	// function it calls is SECURITY DEFINER and joins only rig and repository
	// names, which are not secrets.
	rows, qerr := db.QueryContext(ctx,
		`SELECT rig, repository FROM system_rigs_for_bead($1, $2)`, bead, cell)
	if qerr != nil {
		return "", nil, qerr
	}
	defer rows.Close()

	var found []candidate
	for rows.Next() {
		var c candidate
		if scanErr := rows.Scan(&c.rig, &c.repo); scanErr != nil {
			return "", nil, scanErr
		}
		found = append(found, c)
	}
	if rerr := rows.Err(); rerr != nil {
		return "", nil, rerr
	}

	if rig, refused, decided := Choose(found, wanted); decided {
		return rig, refused, nil
	}

	// None matched. Distinguish the two reasons, because they have different
	// fixes: a project with no registered repository needs one attaching, and a
	// project whose repositories no rig holds needs a rig.
	//
	// Through a SECURITY DEFINER function, and that is the whole point of this
	// call rather than reading work_refs here. This used to be two plain reads,
	// which RLS answered wrongly: with no app user, work_refs shows a row only
	// when project_id IS NULL, so every PROJECT-SCOPED bead looked absent and
	// dispatch reported bead_not_projected with the advice to wait a few
	// seconds. Waiting could never help. Eight beads in project msf were
	// refused that way while sitting in work_refs the whole time.
	//
	// The happy path above was already definer-safe. This path was not, so the
	// failure only appeared once something failed -- and then lied about why.
	var projected bool
	var project string
	var repos int
	if qerr := db.QueryRowContext(ctx,
		`SELECT projected, project, repositories FROM system_bead_routing($1)`,
		bead).Scan(&projected, &project, &repos); qerr != nil {
		return "", nil, qerr
	}

	if !projected {
		// A bead can be real and not yet here: the node projects its graph
		// every fifteen seconds or so, so one created a moment ago has not
		// arrived. The message says that, because "no such bead" would send
		// somebody looking for a typo they did not make.
		//
		// Observed rather than imagined: an unquoted shell variable turned
		// `dispatch $BEAD oss` into `dispatch oss`, and a bead literally named
		// `oss` was accepted, claimed, given to an agent and billed. The agent
		// had nothing to read and nothing to do.
		return "", &Refusal{Code: CodeNotProjected,
			Why: "no bead " + bead + " has been projected from any cell. Either the " +
				"id is wrong, or it was created seconds ago and the next projection " +
				"pass has not run yet -- wait a few seconds and try again. Nothing is " +
				"dispatched against an id that resolves to nothing, because the agent " +
				"would have nothing to read and would be billed for finding that out."}, nil
	}

	if project == "" {
		// Projected, but belonging to no project: nothing to route on, and not
		// an error. A rig the caller named is honoured, and otherwise the
		// dispatcher's default stands.
		//
		// Returning "" for a named rig here would CLEAR it, because the caller
		// assigns this result unconditionally. That is the hazard of turning
		// two paths into one, and it is why this returns `wanted`.
		return strings.TrimSpace(wanted), nil, nil
	}

	if repos == 0 {
		return "", &Refusal{Code: CodeNoRig,
			Why: "the project " + project + " has no repository registered, so there is " +
				"nowhere to run this bead. Attach one with POST /v1/projects/" + project +
				"/repositories."}, nil
	}
	return "", &Refusal{Code: CodeNoRig,
		Why: "no rig in cell " + cell + " holds a repository belonging to " + project +
			". The work would otherwise run in a disposable sandbox and report success " +
			"without doing anything."}, nil
}
