package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Outcome is what a monitor pass did.
//
// Notified and Unaddressed are separate for the reason the witness separates
// them: an alert that reached nobody is a failure that looks exactly like a
// success. The whole point of this work package is detection, so a detection
// path that fails silently would be self-defeating.
type Outcome struct {
	Checked     int
	Failing     int
	Notified    int
	Resolved    int
	Unaddressed int
}

// Publish files each finding: raising an alert for the failing ones, clearing
// the alert for the ones that have recovered.
//
// Clearing matters as much as raising. Without it an operator resolves every
// alert by hand, including the ones whose cause fixed itself, and an inbox full
// of stale alerts is how people learn to stop reading it.
func Publish(ctx context.Context, db *sql.DB, findings []Finding, now time.Time) (Outcome, error) {
	var out Outcome
	for _, f := range findings {
		out.Checked++
		if !f.Failing {
			n, err := resolve(ctx, db, f.Class)
			if err != nil {
				return out, err
			}
			out.Resolved += n
			continue
		}
		out.Failing++

		explanation, err := json.Marshal(map[string]any{
			"source":  "monitor",
			"check":   f.Class.Name,
			"summary": f.Summary,
			// The runbook travels WITH the alert rather than living in a wiki
			// somebody has to know to look in. This is the acceptance criterion
			// for the work package, so it is part of the payload.
			"runbook":  f.Class.Runbook,
			"observed": f.Observed,
			"at":       now.UTC().Format(time.RFC3339),
		})
		if err != nil {
			return out, fmt.Errorf("encoding the explanation for %s: %w", f.Class.Name, err)
		}

		var notified int
		// system_raise_platform_alert, not system_raise_attention: a platform
		// alert belongs to no project, and the project-scoped path deliberately
		// reaches nobody when it cannot resolve one (migration 0020).
		if err := db.QueryRowContext(ctx,
			`SELECT system_raise_platform_alert($1, $2, $3, $4::jsonb)`,
			f.Class.Rule, f.Class.DedupeKey(), f.Class.Score, string(explanation),
		).Scan(&notified); err != nil {
			return out, fmt.Errorf("raising %s: %w", f.Class.Name, err)
		}
		if notified == 0 {
			// The organisation has no admin. Counted rather than dropped:
			// this is the failure mode where the monitor reports success
			// having told no one.
			out.Unaddressed++
		}
		out.Notified += notified
	}
	return out, nil
}

func resolve(ctx context.Context, db *sql.DB, c Class) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT system_resolve_platform_alert($1)`, c.DedupeKey()).Scan(&n); err != nil {
		return 0, fmt.Errorf("resolving %s: %w", c.Name, err)
	}
	return n, nil
}
