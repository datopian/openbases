// Package reconcile recovers projection state that events alone cannot.
//
// Two different failures need two different remedies, and conflating them
// leaves one of them unfixed (plan section 18, WP-D3):
//
//   - A delivery ARRIVED but was not fully projected — the process died between
//     storing the receipt and applying it, or the repository was not registered
//     yet. The receipt exists, so replaying it is enough.
//
//   - A delivery NEVER ARRIVED — the endpoint was down past GitHub's retries,
//     or the event was dropped. There is no receipt, so no amount of replay
//     will help. Only asking GitHub for current state recovers it.
//
// Both are safe to run repeatedly. Projections are keyed upserts guarded on
// updated_at, not appended events, so applying the same fact twice changes
// nothing — which is what "no duplicate domain event effects" requires.
package reconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/datopian/workgraph/internal/githubapp"
)

// Result reports what a reconciliation pass did.
type Result struct {
	Replayed        int // receipts reprocessed
	ReplayFailed    int
	Resynced        int // pull requests refreshed from the API
	ResyncFailed    int
	NotInstalled    int // repositories the App installation does not cover
	StillUnresolved int // receipts that remain unprojectable
}

func (r Result) LogArgs() []any {
	return []any{
		"replayed", r.Replayed, "replay_failed", r.ReplayFailed,
		"resynced", r.Resynced, "resync_failed", r.ResyncFailed,
		"not_installed", r.NotInstalled, "unresolved", r.StillUnresolved,
	}
}

// Replay reprojects deliveries that were received but never applied.
//
// Ordered oldest first so a sequence of events for one pull request lands in
// the order it happened. The ordering guard in the projection would discard an
// out-of-order apply anyway, but replaying oldest-first means the intermediate
// states are also correct, not just the final one.
func Replay(ctx context.Context, db *sql.DB, log *slog.Logger, limit int) (Result, error) {
	var out Result

	// Through the system function, not the table.
	//
	// github_deliveries is protected: the payloads carry titles and branch
	// names from every installed repository. Reconciliation has no user, so a
	// direct read returns nothing — silently, as a pass that reports no work.
	rows, err := db.QueryContext(ctx,
		`SELECT delivery_id, event_type, payload FROM system_pending_deliveries($1)`, limit)
	if err != nil {
		return out, err
	}

	type pending struct {
		id, event string
		payload   []byte
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.event, &p.payload); err != nil {
			rows.Close()
			return out, err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	for _, p := range todo {
		var err error
		switch p.event {
		case "pull_request":
			_, err = githubapp.ProjectPullRequest(ctx, db, p.payload)
		case "check_suite":
			_, err = githubapp.ProjectCheckSuite(ctx, db, p.payload)
		default:
			// An event we do not project. Marking it processed keeps it out of
			// every future pass; leaving it would make the backlog grow without
			// bound and hide the receipts that genuinely need attention.
			if err := markProcessed(ctx, db, p.id); err != nil {
				return out, err
			}
			continue
		}

		switch {
		case errors.Is(err, githubapp.ErrRepositoryNotRegistered):
			// Deliberately left unprocessed. Registering the repository later
			// must be able to recover its history, and that is only possible
			// while the receipt is still pending.
			out.StillUnresolved++
		case err != nil:
			log.Warn("replay failed", "delivery", p.id, "event", p.event, "error", err)
			out.ReplayFailed++
		default:
			if err := markProcessed(ctx, db, p.id); err != nil {
				return out, err
			}
			out.Replayed++
		}
	}
	return out, nil
}

func markProcessed(ctx context.Context, db *sql.DB, id string) error {
	_, err := db.ExecContext(ctx, `SELECT system_mark_delivery_processed($1)`, id)
	return err
}

// apiPullRequest is GitHub's REST shape, reshaped into the webhook shape.
//
// The projection functions take a webhook payload, so the API response is
// rendered into one rather than given its own projection path. Two paths that
// must agree on the same rules would eventually stop agreeing.
type apiPullRequest struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	Merged    bool       `json:"merged"`
	MergedAt  *time.Time `json:"merged_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// Resync asks GitHub for current pull request state.
//
// This is what recovers a delivery that never arrived: with no receipt there is
// nothing to replay, and the only source of truth left is the API.
func Resync(ctx context.Context, db *sql.DB, gh *githubapp.Client, log *slog.Logger) (Result, error) {
	var out Result

	// Likewise through the system function. Reading project_repositories
	// directly returned zero rows and zero failures — a pass that looked
	// healthy and did nothing at all.
	rows, err := db.QueryContext(ctx,
		`SELECT owner, name, provider_id FROM system_github_repositories()`)
	if err != nil {
		return out, err
	}
	type repo struct{ owner, name, providerID string }
	var repos []repo
	for rows.Next() {
		var r repo
		if err := rows.Scan(&r.owner, &r.name, &r.providerID); err != nil {
			rows.Close()
			return out, err
		}
		repos = append(repos, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	for _, r := range repos {
		full := r.owner + "/" + r.name

		// A token scoped to the one repository being reconciled. An unscoped
		// token would work and be one call cheaper, but a bug in this loop
		// would then be able to touch every installed repository.
		tok, err := gh.InstallationToken(ctx, r.name)
		switch {
		case errors.Is(err, githubapp.ErrNotInstalled):
			// The repository is registered to a project but sits in an
			// organisation the App was not installed on. Counted separately and
			// logged at info: it will fail identically on every run, and a
			// scheduled job that is always red is a scheduled job nobody reads.
			log.Info("repository is not covered by the installation", "repository", full)
			out.NotInstalled++
			continue
		case err != nil:
			log.Warn("minting a token for reconciliation", "repository", full, "error", err)
			out.ResyncFailed++
			continue
		}

		prs, err := listPullRequests(ctx, gh, tok, r.owner, r.name)
		if err != nil {
			log.Warn("listing pull requests", "repository", full, "error", err)
			out.ResyncFailed++
			continue
		}

		for _, pr := range prs {
			payload, err := asWebhookPayload(pr, r.providerID, full)
			if err != nil {
				out.ResyncFailed++
				continue
			}
			applied, err := githubapp.ProjectPullRequest(ctx, db, payload)
			switch {
			case errors.Is(err, githubapp.ErrRepositoryNotRegistered):
				// Cannot happen: the repository came from this table. Counted
				// rather than ignored so a schema change that breaks the
				// lookup is visible instead of silent.
				out.ResyncFailed++
			case err != nil:
				log.Warn("projecting during resync", "repository", full, "number", pr.Number, "error", err)
				out.ResyncFailed++
			case applied:
				// Only count a genuine change. A resync that reports work on
				// every pass would make a real divergence impossible to notice.
				out.Resynced++
			}
		}
	}
	return out, nil
}

func asWebhookPayload(pr apiPullRequest, providerID, fullName string) ([]byte, error) {
	repo := map[string]any{"full_name": fullName}
	if providerID != "" {
		var id int64
		if _, err := fmt.Sscan(providerID, &id); err == nil {
			repo["id"] = id
		}
	}
	return json.Marshal(map[string]any{
		"action":       "synchronize",
		"pull_request": pr,
		"repository":   repo,
	})
}

// listPullRequests fetches open and recently closed pull requests.
//
// state=all with a descending update sort, capped: reconciliation exists to
// catch what was missed recently, and walking the entire history of a long-lived
// repository on every pass would cost far more than it recovers.
func listPullRequests(ctx context.Context, gh *githubapp.Client, tok *githubapp.InstallationToken, owner, name string) ([]apiPullRequest, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=all&sort=updated&direction=desc&per_page=50",
		owner, name)

	resp, err := gh.Get(ctx, tok, path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The body is not echoed: it can repeat request detail, and this runs
		// unattended where the output goes straight to a log.
		return nil, fmt.Errorf("listing pull requests: %s", resp.Status)
	}

	var prs []apiPullRequest
	if err := json.NewDecoder(resp.Body).Decode(&prs); err != nil {
		return nil, err
	}
	return prs, nil
}

// replayLockKey is the advisory lock that keeps two replayers out of each
// other's way.
//
// An arbitrary constant, chosen once and never derived from anything that could
// change: an advisory lock is only a lock if everybody asks for the same number.
const replayLockKey int64 = 0x77677265706c6179 // "wgreplay"

// ReplayIfIdle replays pending deliveries unless another replayer is already
// doing it, and reports whether it ran.
//
// Two things call Replay now — the worker every few seconds, and the
// reconciliation timer every fifteen minutes — so they will eventually overlap.
// Without a lock both would read the same pending receipts and project them
// twice. The projection's ordering guard would discard the second apply, so the
// end state would be correct, but each would also mark the delivery processed
// and one would log a failure for work the other had already done. Correct
// results reached through a confusing log is how a real fault becomes invisible.
//
// pg_try_advisory_lock rather than pg_advisory_lock: a replayer that cannot get
// the lock should skip this tick and try again in five seconds, not queue up
// behind a fifteen-minute reconciliation pass and then run against state that
// has already been handled.
//
// The lock is session-scoped, so it is released explicitly and also by the
// connection dropping — a worker killed mid-pass does not leave it held.
func ReplayIfIdle(ctx context.Context, db *sql.DB, log *slog.Logger, limit int) (Result, bool, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return Result{}, false, err
	}
	defer conn.Close()

	var acquired bool
	if err := conn.QueryRowContext(ctx,
		`SELECT pg_try_advisory_lock($1)`, replayLockKey).Scan(&acquired); err != nil {
		return Result{}, false, err
	}
	if !acquired {
		return Result{}, false, nil
	}
	defer func() {
		// Best effort: the lock also dies with the connection, which is what
		// makes this safe if the unlock never runs.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, replayLockKey)
	}()

	out, err := Replay(ctx, db, log, limit)
	return out, true, err
}
