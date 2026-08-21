package monitor

import (
	"fmt"
	"sort"
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
