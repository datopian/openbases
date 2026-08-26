// Command wg-budget sets and inspects spend ceilings.
//
//	wg-budget set bead wg-qw1 200          # 200 cents = $2.00 a day
//	wg-budget set project portaljs-oss 5000
//	wg-budget set cell oss 10000
//	wg-budget status wg-qw1 [--cell oss]
//	wg-budget list
//
// Runs on the control node, where the database is. The dispatcher on an
// execution node does not use this — it asks the control API, which has the
// same answer and no database credential to hand out.
//
// Amounts are in CENTS, not dollars, and the help says so twice because the
// difference between a $2 budget and a 2¢ one is the difference between a
// working day and an immediate stop. They are read as decimal strings and never
// through a float: these are the same numbers usage_records holds at
// numeric(16,8), and a budget that disagrees with the spend it is compared
// against by a rounding error is a budget that trips for no visible reason.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/workgraph/internal/budget"
	"github.com/datopian/workgraph/internal/config"
)

func main() {
	// The subcommand is taken FIRST, then flags are parsed from what follows.
	//
	// Go's flag package stops at the first non-flag argument, so with a plain
	// flag.Parse() the natural `wg-budget status wg-qw1 --cell oss` leaves
	// --cell unparsed and sitting in Args(). The first version of this command
	// did that and answered with a usage error for a command line that reads
	// perfectly well — and had it accepted the arguments instead, the flag
	// would have been silently ignored, which is worse.
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	command := os.Args[1]

	fs := flag.NewFlagSet("wg-budget "+command, flag.ExitOnError)
	cell := fs.String("cell", "", "execution cell, used to resolve a bead that the platform has not seen yet")
	maxAgents := fs.Int("max-agents", 2, "concurrent agent ceiling recorded with the budget")
	maxRuntime := fs.Int("max-runtime-minutes", 0, "per-run wall-clock ceiling recorded with the budget")
	fs.Usage = usage

	// Positional arguments first, flags anywhere after them: walk the tail and
	// split it, so both orderings work rather than one of them failing
	// mysteriously.
	var positional, flags []string
	rest := os.Args[2:]
	for i := 0; i < len(rest); i++ {
		if strings.HasPrefix(rest[i], "-") {
			flags = append(flags, rest[i:]...)
			break
		}
		positional = append(positional, rest[i])
	}
	if err := fs.Parse(flags); err != nil {
		os.Exit(2)
	}
	args := append([]string{command}, positional...)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := open(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()

	switch args[0] {
	case "set":
		if len(args) != 4 {
			fmt.Fprintln(os.Stderr, "usage: wg-budget set <bead|project|cell> <key> <daily-cents>")
			os.Exit(2)
		}
		if err := budget.Set(ctx, db, args[1], args[2], args[3], *maxAgents, *maxRuntime); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("%s %s: %s cents per day\n", args[1], args[2], args[3])

	case "status":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: wg-budget status <bead>")
			os.Exit(2)
		}
		st, err := budget.Read(ctx, db, args[1], *cell)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		d := budget.Decide(st, budget.PolicyFromEnv())
		fmt.Printf("bead        %s\n", args[1])
		if st.SubjectKind == "none" {
			fmt.Printf("budget      none\n")
		} else {
			fmt.Printf("budget      %s %s, %s cents per day\n", st.SubjectKind, st.SubjectKey, budget.Cents(st.DailyCents))
			fmt.Printf("spent       %s cents today, %s remaining\n", budget.Cents(st.SpentCents), budget.Cents(st.RemainingCents))
		}
		fmt.Printf("spend data  %s\n", age(st.StalenessSeconds))
		fmt.Printf("decision    %s — %s\n", allow(d.Allow), d.Reason)
		if d.Warning != "" {
			fmt.Printf("warning     %s\n", d.Warning)
		}
		// Exit code mirrors the decision, so this is usable as a gate too.
		if !d.Allow {
			os.Exit(1)
		}

	case "list":
		if err := list(ctx, db); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	default:
		usage()
		os.Exit(2)
	}
}

func list(ctx context.Context, db *sql.DB) error {
	// Through the system function, NOT a direct SELECT.
	//
	// budget_limits is RLS-protected and this runs with no user, so a direct
	// read returns nothing — while the write beside it succeeds. The first
	// version of this command did exactly that: `set` printed success and
	// `list` printed "no budgets are set", both truthfully. See
	// 0031_budget_listing.sql.
	rows, err := db.QueryContext(ctx, `
		SELECT subject_kind, subject_key, daily_cents::text, max_agents,
		       coalesce(max_runtime_minutes::text, '-'), updated_at
		  FROM system_list_budgets()`)
	if err != nil {
		return err
	}
	defer rows.Close()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SUBJECT\tKEY\tCENTS/DAY\tAGENTS\tMINUTES\tUPDATED")
	n := 0
	for rows.Next() {
		var kind, key, cents, runtime string
		var agents int
		var updated time.Time
		if err := rows.Scan(&kind, &key, &cents, &agents, &runtime, &updated); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", kind, key, budget.Cents(cents), agents, runtime,
			updated.UTC().Format(time.RFC3339))
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	w.Flush()
	if n == 0 {
		// Said explicitly. An empty table printed as a bare header reads like a
		// query that failed, and "no budgets are set" is the single most
		// important thing this command can report.
		fmt.Println("\nno budgets are set: spend is bounded only by the per-gateway pool on Cloudflare")
	}
	return nil
}

func age(seconds int64) string {
	if seconds < 0 {
		return "none has ever been imported"
	}
	return (time.Duration(seconds) * time.Second).Round(time.Second).String() + " old"
}

func allow(ok bool) string {
	if ok {
		return "ALLOW"
	}
	return "REFUSE"
}

func open(ctx context.Context) (*sql.DB, error) {
	dsn := config.DatabaseURL()
	if dsn == "" {
		return nil, errors.New("no database URL is configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func usage() {
	fmt.Fprint(os.Stderr, strings.TrimLeft(`
wg-budget — spend ceilings, in CENTS per day

  wg-budget set bead    <bead-id>      <cents>
  wg-budget set project <project-slug> <cents>
  wg-budget set cell    <cell-slug>    <cents>
  wg-budget status <bead-id> [--cell <slug>]
  wg-budget list

Amounts are CENTS. 200 is two dollars a day; 2 is two cents a day.

The most specific budget wins: a bead's, else its project's, else its cell's.
Spend is summed at the same level, so a project ceiling is compared against the
project's whole spend and not against one bead's.

`, "\n"))
	flag.PrintDefaults()
}
