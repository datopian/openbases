// Command wg-costimport copies AI Gateway spend into usage_records.
//
// Cloudflare is the only place spend exists today, and it is the wrong place to
// keep it: the log rotates on DELETE_OLDEST, so the evidence for any cost claim
// disappears on Cloudflare's schedule rather than ours, and a log entry carries
// no project, so "what did this project cost" cannot be asked at all. This
// program answers both by making the record durable and joinable.
//
// One pass per invocation on a systemd timer, for the reasons wg-monitor is
// built the same way: the timer is externally visible, a crash needs no
// supervision logic here, and a missed run is something the operating system
// records.
//
// Re-running is always safe. Every entry carries the gateway's own id and the
// write is ON CONFLICT DO NOTHING, so an overlapping window costs a few wasted
// round trips and nothing else. The import therefore deliberately re-reads a
// window behind the watermark instead of resuming exactly at it: an entry that
// arrives out of order would otherwise be skipped forever.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/workgraph/internal/config"
	"github.com/datopian/workgraph/internal/cost"
)

func main() {
	var (
		account  = flag.String("account", os.Getenv("CLOUDFLARE_ACCOUNT_ID"), "Cloudflare account id")
		gateways = flag.String("gateways", os.Getenv("WG_COST_GATEWAYS"), "comma-separated AI Gateway names to import")
		overlap  = flag.Duration("overlap", getenvDur("WG_COST_OVERLAP", 2*time.Hour), "re-read this far behind the watermark, so a late-arriving entry is not skipped forever")
		backfill = flag.Duration("backfill", getenvDur("WG_COST_BACKFILL", 90*24*time.Hour), "how far back to read a gateway that has never been imported")
		max      = flag.Int("max", getenvInt("WG_COST_MAX", 20000), "stop after this many entries per gateway in one pass")
		dryRun   = flag.Bool("dry-run", false, "fetch and map, writing nothing")
		apiBase  = flag.String("api-base", getenvStr("WG_COST_API_BASE", defaultAPIBase), "Cloudflare API root")
		jsonOut  = flag.Bool("json", false, "print the summary as JSON")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	token := config.CloudflareAPIToken()

	names := splitList(*gateways)
	switch {
	case *account == "":
		log.Error("no Cloudflare account id; set CLOUDFLARE_ACCOUNT_ID or pass -account")
		os.Exit(2)
	case token == "":
		log.Error("no Cloudflare API token; install the cf_api_token credential or set CLOUDFLARE_API_TOKEN")
		os.Exit(2)
	case len(names) == 0:
		log.Error("no gateways named; set WG_COST_GATEWAYS or pass -gateways")
		os.Exit(2)
	}

	log.Info("starting", "token_source", config.CloudflareAPITokenSource(), "gateways", strings.Join(names, ","))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	db, err := open(ctx)
	if err != nil {
		// Fatal here, unlike the monitor: with no database there is nothing this
		// program can do, and exiting non-zero makes the timer unit fail, which
		// is itself the signal.
		log.Error("the database is unreachable, so nothing can be imported", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	g := gatewayClient{Account: *account, Token: token, Base: *apiBase, HTTP: &http.Client{Timeout: 60 * time.Second}}

	summaries := make([]summary, 0, len(names))
	failed := false
	for _, name := range names {
		s, err := importOne(ctx, db, g, name, *overlap, *backfill, *max, *dryRun)
		if err != nil {
			failed = true
			s.Error = err.Error()
			log.Error("gateway import failed", "gateway", name, "error", err)
		}
		summaries = append(summaries, s)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(summaries)
	} else {
		for _, s := range summaries {
			fmt.Printf("%-32s from=%s fetched=%d imported=%d duplicate=%d skipped=%d complete=%t%s\n",
				s.Gateway, s.From.Format(time.RFC3339), s.Fetched, s.Imported, s.Duplicate, s.Skipped, s.Complete, dryNote(s.DryRun))
			if s.Error != "" {
				fmt.Printf("%-32s error=%s\n", s.Gateway, s.Error)
			}
		}
	}
	if failed {
		os.Exit(1)
	}
}

// summary is one gateway's pass, reported so a run is auditable without the
// database.
type summary struct {
	Gateway   string    `json:"gateway"`
	From      time.Time `json:"from"`
	Fetched   int       `json:"fetched"`
	Imported  int       `json:"imported"`
	Duplicate int       `json:"duplicate"`
	Skipped   int       `json:"skipped"`
	// Complete is false when the pass stopped at -max rather than reaching the
	// start of its window. The next run picks up from the same watermark and
	// reads the same window again, so nothing is lost — but a run that says
	// complete=false has not caught up, and a report built on it is partial.
	Complete bool `json:"complete"`
	// DryRun says the counts were not written. Imported then means "would have
	// been written", which includes entries already present — a dry run cannot
	// tell new from duplicate without doing the write.
	DryRun bool   `json:"dry_run,omitempty"`
	Error  string `json:"error,omitempty"`
}

// dryNote marks a summary that wrote nothing, so a dry run cannot be mistaken
// for a real one in a log.
func dryNote(dry bool) string {
	if dry {
		return " (dry run, nothing written)"
	}
	return ""
}

func importOne(ctx context.Context, db *sql.DB, g gatewayClient, name string, overlap, backfill time.Duration, max int, dryRun bool) (summary, error) {
	s := summary{Gateway: name, DryRun: dryRun}

	var watermark sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT system_usage_watermark($1)`, name).Scan(&watermark); err != nil {
		return s, fmt.Errorf("reading the watermark: %w", err)
	}
	if watermark.Valid {
		s.From = watermark.Time.Add(-overlap)
	} else {
		s.From = time.Now().Add(-backfill)
	}

	entries, complete, err := g.logsSince(ctx, name, s.From, max)
	// Entries fetched before an error are still worth writing: a partial import
	// is idempotent, and discarding them would make a flaky page mean no
	// progress at all.
	s.Fetched, s.Complete = len(entries), complete

	for _, e := range entries {
		r, mapErr := cost.FromEntry(e, name)
		if mapErr != nil {
			var skip cost.ErrSkip
			if errors.As(mapErr, &skip) {
				s.Skipped++
				continue
			}
			// A malformed entry is one bad row, not a failed run.
			s.Skipped++
			continue
		}
		if dryRun {
			s.Imported++
			continue
		}
		var isNew bool
		if wErr := db.QueryRowContext(ctx, `SELECT system_record_usage($1,$2,$3,$4,$5,$6,$7::numeric,$8,$9,$10,$11,$12,$13)`,
			r.ExternalID, r.Gateway, r.Provider, r.Model, r.InputTok, r.OutputTok,
			r.CostCents, r.Cached, r.Succeeded, nullable(r.Role), nullable(r.Cell), nullable(r.Rig), r.Occurred,
		).Scan(&isNew); wErr != nil {
			return s, fmt.Errorf("writing entry %s: %w", r.ExternalID, wErr)
		}
		if isNew {
			s.Imported++
		} else {
			s.Duplicate++
		}
	}
	return s, err
}

// nullable keeps an unattributed field NULL rather than empty string, so
// "nobody tagged this" and "somebody tagged it with nothing" stay distinct.
func nullable(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
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

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getenvDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func getenvStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}
