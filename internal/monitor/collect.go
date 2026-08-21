package monitor

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Collector gathers the observations the evaluators judge.
//
// Gathering is separated from judging so that the judging is testable without a
// database, a socket or a filesystem — the same split the deterministic witness
// uses, and for the same reason: the cases worth testing are the ones that are
// painful to arrange for real.
type Collector struct {
	DB          *sql.DB
	HTTP        *http.Client
	HealthURL   string
	Mounts      []string
	ReceiptDir  string
	Streams     []BackupStream
	ExpectCells int
}

// ProbeAPI asks the control API whether it can serve traffic.
func (c Collector) ProbeAPI(ctx context.Context) APIObservation {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.HealthURL, nil)
	if err != nil {
		return APIObservation{Err: err, Latency: time.Since(start)}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return APIObservation{Err: err, Latency: time.Since(start)}
	}
	defer resp.Body.Close()
	return APIObservation{Status: resp.StatusCode, Latency: time.Since(start)}
}

// ObserveWebhooks measures the inbound delivery queue.
func (c Collector) ObserveWebhooks(ctx context.Context) (WebhookObservation, error) {
	var o WebhookObservation
	var oldest sql.NullFloat64
	// system_webhook_backlog, not a direct SELECT. github_deliveries has
	// row-level security enabled with no policy, so workgraph_app sees an EMPTY
	// table — a direct query would report a healthy queue for ever. Migration
	// 0020 explains it; 0014 records the same mistake being made before.
	err := c.DB.QueryRowContext(ctx,
		`SELECT unprocessed, oldest_seconds FROM system_webhook_backlog()`).
		Scan(&o.Unprocessed, &oldest)
	if err != nil {
		return o, fmt.Errorf("measuring the delivery backlog: %w", err)
	}
	if oldest.Valid {
		o.OldestUnprocessed = time.Duration(oldest.Float64) * time.Second
	}
	return o, nil
}

// ObserveAgentHealth finds when the witness last reported.
func (c Collector) ObserveAgentHealth(ctx context.Context) (AgentHealthObservation, error) {
	o := AgentHealthObservation{Cells: c.ExpectCells}
	var last sql.NullTime
	// Same reason as the backlog above: agent_health_events is protected with no
	// policy, so a direct query would always say "never reported".
	if err := c.DB.QueryRowContext(ctx,
		`SELECT system_agent_health_last_seen()`).Scan(&last); err != nil {
		return o, fmt.Errorf("reading the last agent-health event: %w", err)
	}
	if last.Valid {
		o.LastEventAt = last.Time
	}
	return o, nil
}

// MeasureDisk reads usage for each configured mount point.
//
// A mount that cannot be measured is reported as an error rather than skipped.
// Silently dropping it would turn a broken check into a passing one, which is the
// failure this whole package exists to prevent.
func (c Collector) MeasureDisk() ([]Filesystem, error) {
	out := make([]Filesystem, 0, len(c.Mounts))
	for _, m := range c.Mounts {
		var st syscall.Statfs_t
		// syscall rather than golang.org/x/sys: statfs is all that is needed and
		// it is in the standard library, so this adds no dependency to a binary
		// whose job is to still work when other things are broken.
		if err := syscall.Statfs(m, &st); err != nil {
			return nil, fmt.Errorf("measuring %s: %w", m, err)
		}
		total := st.Blocks * uint64(st.Bsize)
		if total == 0 {
			return nil, fmt.Errorf("measuring %s: reported zero total blocks", m)
		}
		// Bavail, not Bfree: Bfree includes the root-reserved portion, which
		// overstates what is actually usable and would report a filesystem as
		// having room while writes were already failing for the service user.
		avail := st.Bavail * uint64(st.Bsize)
		used := total - avail
		out = append(out, Filesystem{
			Path:        m,
			UsedPercent: float64(used) / float64(total) * 100,
			FreeBytes:   avail,
		})
	}
	return out, nil
}

// ReadBackupReceipts fills in each declared stream's last success from its
// receipt file.
//
// The declaration is the obligation and the receipt is the evidence. A stream
// with no receipt stays Missing, which the evaluator treats as a failure — the
// case a discovery-based check would report as nothing to say.
func (c Collector) ReadBackupReceipts() []BackupStream {
	out := make([]BackupStream, 0, len(c.Streams))
	for _, s := range c.Streams {
		path := filepath.Join(c.ReceiptDir, s.Name+".receipt")
		b, err := os.ReadFile(path)
		if err != nil {
			s.Missing = true
			out = append(out, s)
			continue
		}
		// The receipt's CONTENT is the timestamp, not the file's mtime. A file
		// touched by a copy, a restore or a careless `touch` would otherwise
		// read as a fresh backup.
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
		if err != nil {
			s.Missing = true
			out = append(out, s)
			continue
		}
		s.Last = t
		out = append(out, s)
	}
	return out
}

// Run gathers everything and judges it.
//
// Each check is independent: one that cannot be gathered becomes a FAILING
// finding rather than aborting the pass, so a broken database connection does
// not also hide the disk filling up.
func (c Collector) Run(ctx context.Context, now time.Time, t Thresholds) []Finding {
	findings := []Finding{EvaluateAPI(c.ProbeAPI(ctx))}

	if o, err := c.ObserveWebhooks(ctx); err != nil {
		findings = append(findings, gatherFailed(ClassWebhook, err))
	} else {
		findings = append(findings, EvaluateWebhook(o, t))
	}

	if o, err := c.ObserveAgentHealth(ctx); err != nil {
		findings = append(findings, gatherFailed(ClassAgentStall, err))
	} else {
		findings = append(findings, EvaluateAgentHealth(o, now, t))
	}

	if fs, err := c.MeasureDisk(); err != nil {
		findings = append(findings, gatherFailed(ClassDisk, err))
	} else {
		findings = append(findings, EvaluateDisk(fs, t))
	}

	findings = append(findings, EvaluateBackup(c.ReadBackupReceipts(), now))
	return findings
}

// gatherFailed reports a check that could not be evaluated.
//
// Failing, not skipped. "I could not look" and "I looked and all is well" must
// never produce the same output; collapsing them is how a check stops running
// without anybody noticing.
func gatherFailed(c Class, err error) Finding {
	return Finding{
		Class:    c,
		Failing:  true,
		Summary:  fmt.Sprintf("the %s check could not be evaluated: %v", c.Name, err),
		Observed: map[string]any{"gather_error": err.Error()},
	}
}

// RunWithoutDatabase gathers only what needs no database.
//
// The database-backed checks are reported as ungatherable rather than dropped.
// An operator reading the output of a pass that could not reach the database must
// not see four healthy checks and one failure; they must see which questions went
// unanswered.
func (c Collector) RunWithoutDatabase(ctx context.Context, now time.Time, t Thresholds) []Finding {
	noDB := fmt.Errorf("the database is unreachable")
	findings := []Finding{
		EvaluateAPI(c.ProbeAPI(ctx)),
		gatherFailed(ClassWebhook, noDB),
		gatherFailed(ClassAgentStall, noDB),
	}
	if fs, err := c.MeasureDisk(); err != nil {
		findings = append(findings, gatherFailed(ClassDisk, err))
	} else {
		findings = append(findings, EvaluateDisk(fs, t))
	}
	// Backup receipts are files on disk, so this one still works and is worth
	// having: a database outage during a backup window is exactly when you want
	// to know the backups are current.
	return append(findings, EvaluateBackup(c.ReadBackupReceipts(), now))
}
