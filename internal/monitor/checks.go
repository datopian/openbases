package monitor

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// APIObservation is what a probe of the control API saw.
type APIObservation struct {
	// Status is the HTTP status returned by /health/ready, or 0 if the request
	// did not complete.
	Status int
	// Err is the transport error, if any.
	Err error
	// Latency is how long the request took, whether it succeeded or not.
	Latency time.Duration
}

// EvaluateAPI judges the control API.
//
// /health/ready is the right endpoint rather than /health/live: liveness returns
// 200 from a process that is running but cannot reach the database, which is
// precisely the state the silent password reset left the platform in. A check
// that would have passed through that incident is not worth running.
func EvaluateAPI(o APIObservation) Finding {
	f := Finding{Class: ClassAPI, Observed: map[string]any{
		"status":     o.Status,
		"latency_ms": o.Latency.Milliseconds(),
	}}
	switch {
	case o.Err != nil:
		f.Failing = true
		f.Summary = fmt.Sprintf("the control API did not answer: %v", o.Err)
		f.Observed["error"] = o.Err.Error()
	case o.Status != 200:
		f.Failing = true
		// 503 from this endpoint means the process is up and the database is
		// not, which is a different repair from no answer at all. Saying which
		// is what makes the runbook's first branch decidable.
		f.Summary = fmt.Sprintf("the control API reported not ready (HTTP %d)", o.Status)
	default:
		f.Summary = fmt.Sprintf("ready in %dms", o.Latency.Milliseconds())
	}
	return f
}

// WebhookObservation is the state of the inbound delivery queue.
type WebhookObservation struct {
	// Unprocessed is the number of deliveries with no processed_at.
	Unprocessed int
	// OldestUnprocessed is the age of the oldest such delivery. Zero when there
	// are none.
	OldestUnprocessed time.Duration
}

// EvaluateWebhook judges the delivery backlog.
//
// The trigger is AGE, not depth. A burst of fifty deliveries during a busy merge
// is normal and drains on the next reconciliation pass; one delivery stuck for an
// hour means the consumer is broken. Alerting on depth would fire on success and
// stay quiet during exactly the failure that happened — deliveries accepted, 200
// returned, nothing consuming them.
func EvaluateWebhook(o WebhookObservation, t Thresholds) Finding {
	f := Finding{Class: ClassWebhook, Observed: map[string]any{
		"unprocessed":            o.Unprocessed,
		"oldest_unprocessed_sec": int(o.OldestUnprocessed.Seconds()),
		"threshold_sec":          int(t.WebhookBacklogAge.Seconds()),
	}}
	if o.Unprocessed > 0 && o.OldestUnprocessed > t.WebhookBacklogAge {
		f.Failing = true
		f.Summary = fmt.Sprintf(
			"%d GitHub delivery/deliveries unprocessed, oldest for %s (limit %s)",
			o.Unprocessed, round(o.OldestUnprocessed), t.WebhookBacklogAge)
		return f
	}
	f.Summary = fmt.Sprintf("%d unprocessed, none older than %s", o.Unprocessed, t.WebhookBacklogAge)
	return f
}

// AgentHealthObservation is how recently the witness reported.
type AgentHealthObservation struct {
	// Since is the age of the most recent agent-health event. Zero value for
	// LastEventAt means none has ever arrived.
	LastEventAt time.Time
	// Cells is how many execution cells are expected to be reporting. Zero
	// means none is deployed, in which case silence is correct and not a
	// failure — alerting on an absent component is how a staging environment
	// teaches people to mute alerts.
	Cells int
}

// EvaluateAgentHealth judges whether the witness is still alive.
func EvaluateAgentHealth(o AgentHealthObservation, now time.Time, t Thresholds) Finding {
	f := Finding{Class: ClassAgentStall, Observed: map[string]any{
		"cells":         o.Cells,
		"threshold_sec": int(t.AgentHealthSilence.Seconds()),
	}}
	if o.Cells == 0 {
		f.Summary = "no execution cell is expected to report"
		return f
	}
	if o.LastEventAt.IsZero() {
		f.Failing = true
		f.Summary = fmt.Sprintf(
			"%d execution cell(s) are deployed and no agent-health report has EVER arrived; "+
				"stalled agents would go unreported", o.Cells)
		f.Observed["last_event"] = nil
		return f
	}
	silence := now.Sub(o.LastEventAt)
	f.Observed["silence_sec"] = int(silence.Seconds())
	f.Observed["last_event"] = o.LastEventAt.UTC().Format(time.RFC3339)
	if silence > t.AgentHealthSilence {
		f.Failing = true
		f.Summary = fmt.Sprintf(
			"no agent-health report for %s (limit %s); the witness is not reporting, "+
				"so an empty inbox no longer means healthy agents",
			round(silence), t.AgentHealthSilence)
		return f
	}
	f.Summary = fmt.Sprintf("last agent-health report %s ago", round(silence))
	return f
}

// Filesystem is one mount point's usage.
type Filesystem struct {
	Path        string
	UsedPercent float64
	FreeBytes   uint64
}

// EvaluateDisk judges filesystem pressure across every mount checked.
//
// The worst mount decides, and it is NAMED. "Disk is full" without saying which
// filesystem sends an operator to look at the wrong one, and on this node the
// interesting ones — root and the PostgreSQL data directory — can fill for
// entirely unrelated reasons.
func EvaluateDisk(fs []Filesystem, t Thresholds) Finding {
	f := Finding{Class: ClassDisk, Observed: map[string]any{
		"threshold_percent": t.DiskUsedPercent,
	}}
	if len(fs) == 0 {
		// Not a pass. Being unable to measure is its own failure, and reporting
		// it as healthy is how a broken check becomes invisible.
		f.Failing = true
		f.Summary = "no filesystem could be measured"
		return f
	}
	sorted := make([]Filesystem, len(fs))
	copy(sorted, fs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].UsedPercent > sorted[j].UsedPercent })

	mounts := make([]map[string]any, 0, len(sorted))
	for _, m := range sorted {
		mounts = append(mounts, map[string]any{
			"path": m.Path, "used_percent": m.UsedPercent, "free_bytes": m.FreeBytes,
		})
	}
	f.Observed["mounts"] = mounts

	worst := sorted[0]
	if worst.UsedPercent >= t.DiskUsedPercent {
		f.Failing = true
		f.Summary = fmt.Sprintf("%s is %.1f%% full (limit %.0f%%)",
			worst.Path, worst.UsedPercent, t.DiskUsedPercent)
		return f
	}
	f.Summary = fmt.Sprintf("fullest filesystem %s at %.1f%%", worst.Path, worst.UsedPercent)
	return f
}

// BackupStream is one thing that is supposed to be backed up, and how fresh its
// last success is.
//
// MaxAge is per stream because the streams have genuinely different cadences: a
// Beads graph that changes constantly is not the same obligation as configuration
// that changes monthly.
type BackupStream struct {
	Name    string
	MaxAge  time.Duration
	Last    time.Time // zero means no successful backup is on record
	Missing bool      // true when no receipt exists at all
}

// EvaluateBackup judges every declared backup stream.
//
// Streams are DECLARED rather than discovered. A discovered check reports on the
// backups that exist, which means the total absence of a backup — the failure
// that matters — reads as "nothing to report". Declaring the obligation makes a
// missing stream a failure instead of a silence.
//
// Freshness comes from a receipt written by the backup job only after it has
// verified the remote copy, so the monitor needs no storage credential of its own.
// The receipt is trusted about the upload precisely because it is written after
// that verification rather than after the upload command returned.
func EvaluateBackup(streams []BackupStream, now time.Time) Finding {
	f := Finding{Class: ClassBackup, Observed: map[string]any{}}
	if len(streams) == 0 {
		f.Failing = true
		f.Summary = "no backup stream is declared, so nothing is being checked"
		return f
	}

	report := make([]map[string]any, 0, len(streams))
	var stale []string
	for _, s := range streams {
		entry := map[string]any{"name": s.Name, "max_age_sec": int(s.MaxAge.Seconds())}
		switch {
		case s.Missing || s.Last.IsZero():
			entry["last"] = nil
			entry["state"] = "never"
			stale = append(stale, fmt.Sprintf("%s has never completed", s.Name))
		default:
			age := now.Sub(s.Last)
			entry["last"] = s.Last.UTC().Format(time.RFC3339)
			entry["age_sec"] = int(age.Seconds())
			if age > s.MaxAge {
				entry["state"] = "stale"
				stale = append(stale, fmt.Sprintf("%s last succeeded %s ago (limit %s)",
					s.Name, round(age), s.MaxAge))
			} else {
				entry["state"] = "fresh"
			}
		}
		report = append(report, entry)
	}
	f.Observed["streams"] = report

	if len(stale) > 0 {
		f.Failing = true
		f.Summary = joinAnd(stale)
		return f
	}
	f.Summary = fmt.Sprintf("%d backup stream(s) fresh", len(streams))
	return f
}

// round trims a duration to something a human reads at a glance. An alert saying
// "silent for 41m12.483s" makes the reader do arithmetic before they can act.
func round(d time.Duration) time.Duration {
	switch {
	case d >= time.Hour:
		return d.Round(time.Minute)
	case d >= time.Minute:
		return d.Round(time.Second)
	default:
		return d.Round(100 * time.Millisecond)
	}
}

func joinAnd(items []string) string {
	switch len(items) {
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	}
	out := ""
	for i, s := range items[:len(items)-1] {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out + ", and " + items[len(items)-1]
}

// ServiceState is one unit that is supposed to be running.
//
// DECLARED, like the backup streams and for the same reason: a check that
// reports on the units it can find treats a unit that has vanished entirely as
// nothing to say, and "vanished entirely" is a failure.
type ServiceState struct {
	Name string
	// Active is what systemd says. A unit that is not loaded at all is not
	// active, which is the correct answer to "is this running".
	Active bool
	// Detail carries systemd's own word — active, inactive, failed, unknown —
	// because "not running" and "crashed" send an operator to different places.
	Detail string
}

// EvaluateServices judges whether the declared units are running.
//
// This exists because a downstream symptom is not a liveness check. Event
// freshness depends on workgraph-worker, and when the worker dies the
// reconciliation timer keeps draining the backlog — so deliveries are still
// processed, the webhook check stays green because the backlog never gets old
// enough, and freshness silently degrades from under a minute to the old p50 of
// 459 seconds (wg-95y).
//
// Tightening the webhook threshold would not have caught it either: with the
// worker down the backlog age oscillates between zero and the reconciliation
// period, so any threshold below fifteen minutes flaps and any threshold above
// it never fires.
func EvaluateServices(services []ServiceState) Finding {
	f := Finding{Class: ClassService, Observed: map[string]any{}}
	if len(services) == 0 {
		// Consistent with the backup check: nothing declared means nothing is
		// being watched, and that is worth saying rather than passing.
		f.Failing = true
		f.Summary = "no service is declared, so nothing is being checked"
		return f
	}

	report := make([]map[string]any, 0, len(services))
	var down []string
	for _, s := range services {
		report = append(report, map[string]any{
			"name": s.Name, "active": s.Active, "state": s.Detail,
		})
		if !s.Active {
			down = append(down, fmt.Sprintf("%s is %s", s.Name, orUnknown(s.Detail)))
		}
	}
	f.Observed["services"] = report
	f.Observed["checked"] = len(services)

	if len(down) > 0 {
		f.Failing = true
		// Named, because "a service is down" without saying which sends an
		// operator to look at the wrong one.
		f.Summary = strings.Join(down, "; ")
		return f
	}
	f.Summary = fmt.Sprintf("%d service(s) running", len(services))
	return f
}

// GatewayImport is the last successful import for one gateway.
type GatewayImport struct {
	Name string
	// Last is when the importer last completed a pass for this gateway. Zero
	// means it never has.
	Last time.Time
	// Complete is false when the pass stopped at its per-run cap rather than
	// reaching the start of its window. Such a run has not caught up, and a
	// budget resting on it is resting on a partial read.
	Complete bool
}

// EvaluateCostImport judges whether spend is still being imported.
//
// Keyed off the last successful RUN rather than the newest usage record. A
// gateway nobody has used for a week has a newest record a week old however
// punctually the importer ran, so measuring the record alerts loudest on the
// quietest environments — the same mistake the budget check made until 0033, and
// it made every dispatch refuse (wg-7jz).
func EvaluateCostImport(imports []GatewayImport, now time.Time, t Thresholds) Finding {
	f := Finding{Class: ClassCostImport, Observed: map[string]any{
		"max_age_sec": int(t.CostImportMaxAge.Seconds()),
	}}
	if len(imports) == 0 {
		f.Failing = true
		f.Summary = "no gateway import has ever been recorded, so spend is not being imported at all"
		return f
	}

	report := make([]map[string]any, 0, len(imports))
	var stale []string
	var partial []string
	for _, g := range imports {
		entry := map[string]any{"gateway": g.Name}
		switch {
		case g.Last.IsZero():
			entry["last"] = nil
			entry["state"] = "never"
			stale = append(stale, fmt.Sprintf("%s has never been imported", g.Name))
		default:
			age := now.Sub(g.Last)
			entry["last"] = g.Last.UTC().Format(time.RFC3339)
			entry["age_sec"] = int(age.Seconds())
			if age > t.CostImportMaxAge {
				entry["state"] = "stale"
				stale = append(stale, fmt.Sprintf("%s last imported %s ago (limit %s)",
					g.Name, age.Round(time.Minute), t.CostImportMaxAge))
			} else if !g.Complete {
				// Not stale and not healthy: the importer is running and has
				// not caught up. Reported, because a budget decision resting on
				// a partial read is resting on the wrong number.
				entry["state"] = "incomplete"
				partial = append(partial, g.Name)
			} else {
				entry["state"] = "fresh"
			}
		}
		report = append(report, entry)
	}
	f.Observed["gateways"] = report

	if len(stale) > 0 {
		f.Failing = true
		f.Summary = strings.Join(stale, "; ")
		return f
	}
	if len(partial) > 0 {
		f.Failing = true
		f.Summary = fmt.Sprintf("import has not caught up for %s", strings.Join(partial, ", "))
		return f
	}
	f.Summary = fmt.Sprintf("%d gateway(s) imported within %s", len(imports), t.CostImportMaxAge)
	return f
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// WorkspaceSource is one allow-listed Google Workspace source and what we
// believe about its subscription.
type WorkspaceSource struct {
	Name    string
	Kind    string
	Enabled bool
	// State is our record of Google's lifecycle state, or empty when no
	// subscription exists.
	State     string
	ExpiresAt time.Time
	LastError string
}

// EvaluateWorkspaceSources judges every allow-listed source's subscription.
//
// It deliberately does NOT check whether events are arriving. A Meet source
// legitimately produces nothing for days — the meeting is Mon-Thu and is
// sometimes skipped or held with transcription off — and a Drive source is quiet
// at weekends. An alert on delivery silence would fire during correct operation,
// which is how alerting systems get muted in week one.
//
// What it checks is the thing that is never healthy: an enabled source whose
// subscription is missing, not active, already expired, or closer to expiry than
// the reconciler's own renewal window. Each of those means a pass that should
// have renewed it did not run or did not succeed — and the consequence is not a
// backlog. Events that arrive with no live subscription are never sent at all.
func EvaluateWorkspaceSources(sources []WorkspaceSource, now time.Time, t Thresholds) Finding {
	f := Finding{Class: ClassWorkspaceSources, Observed: map[string]any{}}

	// Declared, like backup streams: a discovered check reports on the
	// subscriptions that exist, so an empty allow-list — migrations not applied,
	// or the table read through the wrong path — reads as "nothing to report".
	if len(sources) == 0 {
		f.Failing = true
		f.Summary = "no Google Workspace source is allow-listed, so nothing is being ingested"
		return f
	}

	report := make([]map[string]any, 0, len(sources))
	var problems []string
	enabled := 0

	for _, s := range sources {
		entry := map[string]any{"name": s.Name, "kind": s.Kind, "enabled": s.Enabled}
		if !s.Enabled {
			// A disabled source is a decision, not a fault. Its subscription
			// having been deleted is the correct end state.
			entry["state"] = "disabled"
			report = append(report, entry)
			continue
		}
		enabled++
		entry["state"] = s.State
		if s.LastError != "" {
			entry["last_error"] = s.LastError
		}

		switch {
		case s.State == "" || s.State == "pending":
			problems = append(problems, s.Name+" has no subscription")

		case s.State != "active":
			// suspended, failed or deleted. Each has its own repair in the
			// reconciler, and each means nothing is arriving now.
			msg := s.Name + " is " + s.State
			if s.LastError != "" {
				msg += " (" + firstLine(s.LastError) + ")"
			}
			problems = append(problems, msg)

		case s.ExpiresAt.IsZero():
			// Active with no expiry is not a subscription anything can reason
			// about, including the renewer.
			problems = append(problems, s.Name+" is active with no recorded expiry")

		case !s.ExpiresAt.After(now):
			entry["expired_for_sec"] = int(now.Sub(s.ExpiresAt).Seconds())
			problems = append(problems, fmt.Sprintf("%s expired %s ago", s.Name, round(now.Sub(s.ExpiresAt))))

		case s.ExpiresAt.Sub(now) <= t.SubscriptionRenewalGrace:
			// Inside the renewal window for longer than the reconciler should
			// need. Not yet lost, which is the point of alerting here.
			entry["expires_in_sec"] = int(s.ExpiresAt.Sub(now).Seconds())
			problems = append(problems, fmt.Sprintf("%s expires in %s and has not been renewed (limit %s)",
				s.Name, round(s.ExpiresAt.Sub(now)), t.SubscriptionRenewalGrace))

		default:
			entry["expires_in_sec"] = int(s.ExpiresAt.Sub(now).Seconds())
		}
		if !s.ExpiresAt.IsZero() {
			entry["expires_at"] = s.ExpiresAt.UTC().Format(time.RFC3339)
		}
		report = append(report, entry)
	}

	f.Observed["sources"] = report
	f.Observed["enabled"] = enabled

	if enabled == 0 {
		f.Failing = true
		f.Summary = "every Google Workspace source is disabled, so nothing is being ingested"
		return f
	}
	if len(problems) > 0 {
		f.Failing = true
		f.Summary = joinAnd(problems)
		return f
	}
	f.Summary = fmt.Sprintf("%d Workspace subscription(s) active and renewed", enabled)
	return f
}

// firstLine trims a stored error to its first line for a summary. Google's
// errors are multi-line JSON, and an alert summary that carries all of it is one
// nobody reads.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return strings.TrimSpace(s)
}

// Graph is one work graph, as observed on disk.
type Graph struct {
	// Name is how the report refers to it, e.g. "company-hq".
	Name string
	// Path is the graph directory.
	Path string
	// Readable is whether the SERVICE USER can read the graph, not whether
	// root can. That distinction is the entire check: on 3 September the files
	// were present, intact, and root-owned, so every check performed as root
	// would have reported a healthy graph while the service could not open it.
	Readable bool
	// Reason is why not, when Readable is false.
	Reason string
	// Missing is true when the graph directory does not exist at all, which is
	// different from present-and-unreadable and has a different fix.
	Missing bool
	// Uninitialised is true when the directory exists and holds no Dolt
	// database yet.
	//
	// NOT a failure. beads_hq provisions a graph directory and Dolt creates its
	// database on first write, so a declared graph nobody has written to is a
	// normal state — two of the three graphs on staging are in it. The first
	// version of this check called that an outage.
	Uninitialised bool
}

// EvaluateGraphs judges whether the work graphs can be read by the service.
//
// Graphs are DECLARED rather than discovered, for the same reason backup
// streams are: a discovered check reports on the graphs that are present, so a
// graph that has vanished reads as "nothing to report" — and a vanished work
// graph is the worst outcome of the set.
func EvaluateGraphs(graphs []Graph) Finding {
	f := Finding{Class: ClassGraph, Observed: map[string]any{}}

	if len(graphs) == 0 {
		// Not a pass. Declaring no graph means the check is switched off, and
		// "switched off" must not look like "healthy" — that is the same
		// collapse the backup check refuses to make.
		f.Failing = true
		f.Summary = "no work graph is declared, so nothing checks that the graph can be read"
		f.Observed["graphs"] = []any{}
		return f
	}

	entries := make([]map[string]any, 0, len(graphs))
	var broken []string
	for _, g := range graphs {
		entry := map[string]any{"name": g.Name, "path": g.Path}
		switch {
		case g.Missing:
			entry["state"] = "missing"
			broken = append(broken, fmt.Sprintf("%s is not on disk at %s", g.Name, g.Path))
		case g.Uninitialised:
			// Reported, not failed. Worth seeing — a graph that stays empty for
			// weeks may be a graph nothing is writing to — but it is not the
			// present outage this class exists for.
			entry["state"] = "uninitialised"
		case !g.Readable:
			entry["state"] = "unreadable"
			entry["reason"] = g.Reason
			broken = append(broken, fmt.Sprintf("%s cannot be read by the service user: %s",
				g.Name, g.Reason))
		default:
			entry["state"] = "readable"
		}
		entries = append(entries, entry)
	}
	f.Observed["graphs"] = entries

	if len(broken) > 0 {
		f.Failing = true
		f.Summary = strings.Join(broken, "; ")
		return f
	}
	var ready, empty int
	for _, g := range graphs {
		if g.Uninitialised {
			empty++
		} else {
			ready++
		}
	}
	if empty > 0 {
		f.Summary = fmt.Sprintf("%d work graph(s) readable by the service user, %d not yet initialised",
			ready, empty)
		return f
	}
	f.Summary = fmt.Sprintf("%d work graph(s) readable by the service user", ready)
	return f
}
