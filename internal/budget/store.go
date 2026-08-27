package budget

import (
	"context"
	"database/sql"
	"fmt"
)

// Read asks the database which budget applies to a bead and what has been spent
// against it.
//
// One round trip to one SECURITY DEFINER function, because the resolution order
// and the summation have to agree and doing either of them here would be a
// second place for them to disagree. See 0030_budget_status.sql.
func Read(ctx context.Context, db *sql.DB, bead, cell string) (Status, error) {
	var (
		s         Status
		key       sql.NullString
		daily     sql.NullString
		remaining sql.NullString
		newest    sql.NullTime
		staleness sql.NullInt64
		agents    sql.NullInt64
		runtime   sql.NullInt64
	)
	err := db.QueryRowContext(ctx,
		`SELECT subject_kind, subject_key, daily_cents::text, spent_cents::text,
		        remaining_cents::text, exceeded, newest_record_at, staleness_seconds,
		        max_agents, max_runtime_minutes
		   FROM system_budget_status($1, $2)`,
		bead, nullable(cell),
	).Scan(&s.SubjectKind, &key, &daily, &s.SpentCents, &remaining, &s.Exceeded, &newest, &staleness,
		&agents, &runtime)
	if err != nil {
		return Status{}, fmt.Errorf("reading the budget for %s: %w", bead, err)
	}

	s.MaxAgents = int(agents.Int64)
	s.MaxRuntimeMinutes = int(runtime.Int64)
	s.SubjectKey = key.String
	s.DailyCents = daily.String
	s.RemainingCents = remaining.String
	if staleness.Valid {
		s.StalenessSeconds = staleness.Int64
	} else {
		// Nothing has ever been imported. Negative rather than zero, because
		// zero is the freshest possible answer and this is the least
		// trustworthy one — the distinction the decision layer acts on.
		s.StalenessSeconds = -1
	}
	return s, nil
}

// Set writes a budget for a bead, project or cell.
func Set(ctx context.Context, db *sql.DB, kind, key, dailyCents string, maxAgents int, maxRuntimeMinutes int) error {
	var runtime any
	if maxRuntimeMinutes > 0 {
		runtime = maxRuntimeMinutes
	}
	var ok bool
	if err := db.QueryRowContext(ctx,
		`SELECT system_set_budget($1, $2, $3::numeric, $4, $5)`,
		kind, key, dailyCents, maxAgents, runtime,
	).Scan(&ok); err != nil {
		return fmt.Errorf("setting the %s budget for %s: %w", kind, key, err)
	}
	return nil
}

// Unset removes a budget so the subject falls back to the one above it: a bead
// to its project, a project to its cell. Reports whether there was one.
//
// Set alone could not express this. Zero is a real budget meaning "refuse
// everything", so it is not a way to remove one, and raising the number instead
// leaves a limit in place that hides the fallback.
func Unset(ctx context.Context, db *sql.DB, kind, key string) (bool, error) {
	var existed bool
	if err := db.QueryRowContext(ctx,
		`SELECT system_clear_budget($1, $2)`, kind, key,
	).Scan(&existed); err != nil {
		return false, fmt.Errorf("clearing the %s budget for %s: %w", kind, key, err)
	}
	return existed, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
