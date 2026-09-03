package monitor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
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
	// Services are the systemd units that must be running (wg-95y).
	Services []string
	// Gateways are the AI Gateways whose import must be fresh (wg-7jz).
	Gateways []string
	// Graphs are the work graph directories that must be readable, declared as
	// "name=/path" pairs (wg-7bh).
	Graphs []Graph
	// GraphUser is the account the graphs must be readable BY. Empty means the
	// process's own user, which is only the right answer when the monitor runs
	// as the service user.
	GraphUser string
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
// ObserveWorkspaceSources reads every allow-listed source and its subscription.
//
// Through system_event_sources() rather than the tables, because all three are
// behind FORCE ROW LEVEL SECURITY and the monitor has no user identity. Reading
// the table directly would return nothing, which the evaluator would correctly
// report as "no source is allow-listed" — a true statement about what this
// connection can see and a false one about the system.
func (c Collector) ObserveWorkspaceSources(ctx context.Context) ([]WorkspaceSource, error) {
	if c.DB == nil {
		return nil, errors.New("no database")
	}
	rows, err := c.DB.QueryContext(ctx,
		`SELECT display_name, kind, enabled, state, expires_at FROM system_event_sources()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WorkspaceSource
	for rows.Next() {
		var w WorkspaceSource
		var state sql.NullString
		var expires sql.NullTime
		if err := rows.Scan(&w.Name, &w.Kind, &w.Enabled, &state, &expires); err != nil {
			return nil, err
		}
		w.State = state.String
		if expires.Valid {
			w.ExpiresAt = expires.Time
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The last error is a separate read because system_event_sources() does not
	// carry it: it exists to drive reconciliation, and an error message is for
	// humans. Failing here is not fatal — a subscription in a bad state is worth
	// alerting on with or without the reason.
	errRows, err := c.DB.QueryContext(ctx,
		`SELECT s.display_name, sub.last_error
		   FROM event_subscriptions sub
		   JOIN event_sources s ON s.id = sub.source_id
		  WHERE sub.last_error IS NOT NULL`)
	if err == nil {
		defer errRows.Close()
		reasons := map[string]string{}
		for errRows.Next() {
			var name, reason string
			if err := errRows.Scan(&name, &reason); err == nil {
				reasons[name] = reason
			}
		}
		for i := range out {
			out[i].LastError = reasons[out[i].Name]
		}
	}
	return out, nil
}

func (c Collector) Run(ctx context.Context, now time.Time, t Thresholds) []Finding {
	findings := []Finding{EvaluateAPI(c.ProbeAPI(ctx))}

	// Needs no database, so it runs in both paths and is the one check that
	// still works when everything else is ungatherable — which is when knowing
	// what is running matters most.
	findings = append(findings, EvaluateServices(c.ObserveServices(ctx)))

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

	if imports, err := c.ObserveCostImports(ctx); err != nil {
		findings = append(findings, gatherFailed(ClassCostImport, err))
	} else {
		findings = append(findings, EvaluateCostImport(imports, now, t))
	}

	if sources, err := c.ObserveWorkspaceSources(ctx); err != nil {
		findings = append(findings, gatherFailed(ClassWorkspaceSources, err))
	} else {
		findings = append(findings, EvaluateWorkspaceSources(sources, now, t))
	}

	findings = append(findings, EvaluateBackup(c.ReadBackupReceipts(), now))
	findings = append(findings, EvaluateGraphs(c.ObserveGraphs(ctx)))
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
		// Asking systemd needs no database, and a database outage is exactly
		// when "which units are actually running" is the useful question —
		// postgresql.service being one of them.
		EvaluateServices(c.ObserveServices(ctx)),
		gatherFailed(ClassWebhook, noDB),
		gatherFailed(ClassAgentStall, noDB),
		gatherFailed(ClassCostImport, noDB),
		gatherFailed(ClassWorkspaceSources, noDB),
	}
	if fs, err := c.MeasureDisk(); err != nil {
		findings = append(findings, gatherFailed(ClassDisk, err))
	} else {
		findings = append(findings, EvaluateDisk(fs, t))
	}
	// Backup receipts are files on disk, so this one still works and is worth
	// having: a database outage during a backup window is exactly when you want
	// to know the backups are current.
	findings = append(findings, EvaluateBackup(c.ReadBackupReceipts(), now))
	// So is the graph check, and more so: the work graph is a separate database
	// from PostgreSQL, and "PostgreSQL is down" is no reason to stop asking
	// whether the work graph can be opened.
	return append(findings, EvaluateGraphs(c.ObserveGraphs(ctx)))
}

// ObserveGraphs tests whether each declared graph can be read by the service
// user.
//
// The readability test runs AS THAT USER rather than as the monitor. That is
// the whole point: on 3 September the graph files were present and intact and
// root-owned, so any check performed as root would have reported a healthy
// graph while the service could not open its own database. `sudo -u <user> test
// -r` asks the question that actually matters.
func (c Collector) ObserveGraphs(ctx context.Context) []Graph {
	out := make([]Graph, 0, len(c.Graphs))
	for _, g := range c.Graphs {
		// The manifest specifically, not the directory. A directory can be
		// traversable while the file the database needs is not readable, which
		// is exactly the shape the 3 September failure took:
		//   open .../noms/manifest: permission denied
		manifest := filepath.Join(g.Path, ".beads", "embeddeddolt", "wg", ".dolt", "noms", "manifest")
		if _, err := os.Stat(g.Path); err != nil {
			g.Missing = true
			g.Reason = err.Error()
			out = append(out, g)
			continue
		}
		if _, err := os.Stat(manifest); err != nil {
			// Present directory, absent manifest. Not "missing" — the graph is
			// there and its database is not, which is a different fix.
			g.Readable = false
			g.Reason = "the Dolt manifest is not present: " + err.Error()
			out = append(out, g)
			continue
		}
		if c.GraphUser == "" {
			// No user declared, so the best available answer is whether THIS
			// process can read it. Recorded in the reason so the report does
			// not overclaim.
			if err := readable(manifest); err != nil {
				g.Reason = "not readable by the monitor's own user (no service user declared): " + err.Error()
			} else {
				g.Readable = true
			}
			out = append(out, g)
			continue
		}
		cmd := exec.CommandContext(ctx, "sudo", "-n", "-u", c.GraphUser, "test", "-r", manifest)
		if err := cmd.Run(); err != nil {
			g.Reason = fmt.Sprintf("%s cannot read %s", c.GraphUser, manifest)
		} else {
			g.Readable = true
		}
		out = append(out, g)
	}
	return out
}

// readable reports whether this process can open a file for reading.
func readable(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// ObserveServices asks systemd whether each declared unit is running (wg-95y).
//
// systemctl rather than a D-Bus client: the monitor already runs on the node as
// a systemd oneshot, `is-active` is a stable interface, and a dependency on a
// D-Bus library to answer a question a shell command answers is not worth
// carrying.
//
// A non-zero exit is the ANSWER, not an error. `is-active` exits 3 for an
// inactive unit and prints its state, which is precisely what is being asked;
// treating that as a failure to check would turn every real outage into "could
// not be evaluated".
func (c Collector) ObserveServices(ctx context.Context) []ServiceState {
	out := make([]ServiceState, 0, len(c.Services))
	for _, name := range c.Services {
		cmd := exec.CommandContext(ctx, "systemctl", "is-active", name)
		raw, _ := cmd.Output()
		state := strings.TrimSpace(string(raw))
		out = append(out, ServiceState{
			Name:   name,
			Active: state == "active",
			Detail: state,
		})
	}
	return out
}

// ObserveCostImports reads the last successful import per gateway (wg-7jz).
//
// usage_import_runs holds no project data — only "gateway X was read at time T"
// — so it carries no row-level security and needs no system function to read,
// unlike almost everything else the monitor touches.
//
// Gateways are DECLARED and then looked up, rather than being discovered from
// the table. A gateway that has never been imported has no row at all, and
// discovery would report that as nothing to say — which is the failure that
// matters most, exactly as with the backup streams.
func (c Collector) ObserveCostImports(ctx context.Context) ([]GatewayImport, error) {
	if c.DB == nil {
		return nil, errors.New("no database")
	}
	out := make([]GatewayImport, 0, len(c.Gateways))
	for _, name := range c.Gateways {
		g := GatewayImport{Name: name}
		var ran sql.NullTime
		var complete sql.NullBool
		err := c.DB.QueryRowContext(ctx,
			`SELECT ran_at, complete FROM usage_import_runs WHERE gateway = $1`, name).
			Scan(&ran, &complete)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// Left zero: never imported, which the evaluator reports as its own
			// state rather than as staleness.
		case err != nil:
			return nil, err
		default:
			if ran.Valid {
				g.Last = ran.Time
			}
			g.Complete = complete.Bool
		}
		out = append(out, g)
	}
	return out, nil
}
