// Command wg-monitor checks the platform and files what it finds.
//
// One pass per invocation, driven by a systemd timer rather than an internal
// loop. A timer is externally visible (systemctl list-timers says when it last
// ran and when it runs next), it restarts after a crash without supervision
// logic here, and a missed run is a fact the operating system records rather
// than something this process would have to notice about itself.
//
// The blind spot is stated rather than hidden: this runs ON the node and reports
// THROUGH the database. If the whole node is down, or the database is
// unreachable, it cannot deliver an alert about that — the case where alerting
// is most needed is the one an in-band monitor cannot cover. It writes those
// failures to the journal at a distinguishable priority so a log-based external
// alarm can find them, and closing the gap properly needs an external prober
// (tracked separately).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/workgraph/internal/config"
	"github.com/datopian/workgraph/internal/monitor"
)

func main() {
	var (
		healthURL  = flag.String("health-url", getenv("WG_MONITOR_HEALTH_URL", "http://127.0.0.1:8080/health/ready"), "control API readiness endpoint")
		mounts     = flag.String("mounts", getenv("WG_MONITOR_MOUNTS", "/"), "comma-separated mount points to measure")
		receiptDir = flag.String("receipts", getenv("WG_MONITOR_RECEIPT_DIR", "/var/lib/workgraph/backups"), "directory holding backup receipts")
		streams    = flag.String("backup-streams", getenv("WG_MONITOR_BACKUP_STREAMS", "beads:24h"), "comma-separated name:max-age backup obligations")
		cells      = flag.Int("expect-cells", getenvInt("WG_MONITOR_EXPECT_CELLS", 0), "how many execution cells should be reporting agent health")
		dryRun     = flag.Bool("dry-run", false, "evaluate and print, raising nothing")
		// Thresholds are flags, not constants. The right value differs between
		// staging and production, a threshold that needs a release to change is
		// one that gets worked around instead — and the acceptance test needs to
		// be able to induce each failure without filling a real disk.
		diskPct    = flag.Float64("disk-percent", getenvFloat("WG_MONITOR_DISK_PERCENT", monitor.DefaultThresholds().DiskUsedPercent), "alert when a filesystem is this full")
		backlogAge = flag.Duration("webhook-backlog-age", getenvDur("WG_MONITOR_WEBHOOK_BACKLOG_AGE", monitor.DefaultThresholds().WebhookBacklogAge), "alert when a delivery is unprocessed for this long")
		silence    = flag.Duration("agent-health-silence", getenvDur("WG_MONITOR_AGENT_SILENCE", monitor.DefaultThresholds().AgentHealthSilence), "alert when no agent-health report arrives for this long")
		jsonOut    = flag.Bool("json", false, "print findings as JSON")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	parsedStreams, err := parseStreams(*streams)
	if err != nil {
		log.Error("the backup declaration could not be parsed", "error", err)
		os.Exit(2)
	}

	// The database is opened before the checks run, and a failure to open it is
	// NOT fatal. The API check is still worth running and reporting to the
	// journal: "the database is unreachable" is the single most valuable thing
	// this program can say, and exiting early would say it only to the exit
	// code.
	var db *sql.DB
	dsn := config.DatabaseURL()
	if dsn == "" {
		log.Error("ALERT-UNDELIVERABLE no database URL is configured; the monitor cannot file anything")
	} else if db, err = sql.Open("pgx", dsn); err != nil {
		log.Error("ALERT-UNDELIVERABLE the database driver refused the connection string", "error", err)
		db = nil
	} else {
		defer db.Close()
		db.SetMaxOpenConns(2)
		if err := db.PingContext(ctx); err != nil {
			log.Error("ALERT-UNDELIVERABLE the database is unreachable, so no alert can be filed", "error", err)
			db.Close()
			db = nil
		}
	}

	c := monitor.Collector{
		DB:          db,
		HTTP:        &http.Client{Timeout: 10 * time.Second},
		HealthURL:   *healthURL,
		Mounts:      splitNonEmpty(*mounts),
		ReceiptDir:  *receiptDir,
		Streams:     parsedStreams,
		ExpectCells: *cells,
	}

	thresholds := monitor.Thresholds{
		WebhookBacklogAge:  *backlogAge,
		AgentHealthSilence: *silence,
		DiskUsedPercent:    *diskPct,
	}

	now := time.Now()
	var findings []monitor.Finding
	if db == nil {
		// Without a database only the checks that need no database can run. The
		// rest are reported as ungatherable rather than omitted, so the output
		// distinguishes "not checked" from "healthy".
		findings = c.RunWithoutDatabase(ctx, now, thresholds)
	} else {
		findings = c.Run(ctx, now, thresholds)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(findings)
	} else {
		for _, f := range findings {
			state := "ok"
			if f.Failing {
				state = "FAILING"
			}
			fmt.Printf("%-12s %-8s %s\n", f.Class.Name, state, f.Summary)
		}
	}

	failing := 0
	for _, f := range findings {
		if f.Failing {
			failing++
		}
	}

	if *dryRun || db == nil {
		if db == nil && failing > 0 {
			// Every failing finding, spelled out in the journal with its
			// runbook, because this is the only channel left.
			for _, f := range findings {
				if f.Failing {
					log.Error("ALERT-UNDELIVERABLE "+f.Class.Name,
						"summary", f.Summary, "runbook", f.Class.Runbook)
				}
			}
			os.Exit(1)
		}
		os.Exit(0)
	}

	out, err := monitor.Publish(ctx, db, findings, now)
	if err != nil {
		log.Error("filing the findings failed", "error", err)
		os.Exit(1)
	}
	log.Info("monitor pass complete",
		"checked", out.Checked, "failing", out.Failing,
		"notified", out.Notified, "resolved", out.Resolved,
		"unaddressed", out.Unaddressed)

	// An alert that reached nobody is a failure that looks like a success, so it
	// is the one thing here that makes the unit fail — a red unit in
	// `systemctl list-units --failed` is itself a signal, and it is the last one
	// available when the inbox is not working.
	if out.Unaddressed > 0 {
		log.Error("ALERT-UNDELIVERABLE some alerts reached no recipient; "+
			"check that the platform project exists and the organisation has an admin",
			"unaddressed", out.Unaddressed)
		os.Exit(1)
	}
}

// parseStreams reads "name:duration" pairs, e.g. "beads:24h,postgres:24h".
func parseStreams(s string) ([]monitor.BackupStream, error) {
	var out []monitor.BackupStream
	for _, part := range splitNonEmpty(s) {
		name, age, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("%q is not name:max-age", part)
		}
		d, err := time.ParseDuration(age)
		if err != nil {
			return nil, fmt.Errorf("the max age for %q: %w", name, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("the max age for %q must be positive", name)
		}
		out = append(out, monitor.BackupStream{Name: name, MaxAge: d})
	}
	return out, nil
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func getenvDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func getenvInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
