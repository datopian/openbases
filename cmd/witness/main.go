// Command witness watches polecat health without asking a model.
//
// It replaces Gas Town's Witness patrol, which is a Claude Code session driven
// by a long prose formula on a 300-second cycle. The patrol's four jobs —
// detect stalled polecats, nudge unresponsive sessions, clean up zombies, nuke
// completed sandboxes — are mechanical, and Gas Town already computes the
// answers in Go. Paying a frontier model to read a boolean is what made the
// patrol roles cost more than the work they were watching.
//
// Run this on the execution node, as the cell user, for as long as the town is
// up. It settles the routine cases itself and escalates anything that could
// lose work to the attention inbox, where a person decides.
//
// The stock patrol must be switched off for this to be the only witness: the
// town's settings/config.json needs "disabled_patrols": ["witness"], which the
// gastown Ansible role writes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/datopian/workgraph/internal/witness"
)

func main() {
	var (
		town     = flag.String("town", "", "the Gas Town directory, e.g. /srv/cells/oss/town")
		cell     = flag.String("cell", "", "the security domain this town belongs to, e.g. oss")
		gtPath   = flag.String("gt", "/usr/local/bin/gt", "the pinned gt binary")
		interval = flag.Duration("interval", 45*time.Second, "how often to look for new events")
		// The safety sweep exists because not every transition emits an event.
		// A session killed by the OOM killer leaves no POLECAT_DONE behind, and
		// a witness that only reacts to events would never notice it.
		sweep    = flag.Duration("sweep", 10*time.Minute, "run a full pass even with no events")
		stall    = flag.Duration("stall-after", witness.StallAfter, "silence before a live session is called ambiguous")
		endpoint = flag.String("control-api", "", "base URL of the control API; escalations are logged only if empty")
		once     = flag.Bool("once", false, "run a single pass and exit")
		dryRun   = flag.Bool("dry-run", false, "decide and report, but take no action")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if *town == "" || *cell == "" {
		log.Error("both -town and -cell are required")
		os.Exit(2)
	}
	if _, err := os.Stat(*town); err != nil {
		log.Error("the town directory is not readable", "town", *town, "error", err)
		os.Exit(1)
	}

	t := witness.Town{Root: *town, GT: *gtPath, Timeout: 2 * time.Minute}
	c := &controlAPI{
		base:   *endpoint,
		// The names the cell's credential file actually uses. Reading
		// WG_SERVICE_TOKEN_* instead was a real defect: the service ran, made
		// its decisions correctly, logged its escalations to the journal — and
		// never reached the inbox, reporting only "the token is not set" in a
		// line nobody was watching. It passed a manual test because the test
		// exported the names the code wanted.
		token:  os.Getenv("WG_ACCESS_CLIENT_ID"),
		secret: os.Getenv("WG_ACCESS_CLIENT_SECRET"),
		client: &http.Client{Timeout: 30 * time.Second},
	}

	// Refuse to start half-configured. A witness told where the control plane
	// is but given no credential for it runs perfectly, decides correctly, and
	// escalates to a journal nobody reads — which is indistinguishable from a
	// healthy witness with nothing to report. That happened.
	if *endpoint != "" && (c.token == "" || c.secret == "") {
		log.Error("a control API was given but its credentials are missing",
			"control_api", *endpoint,
			"needs", "WG_ACCESS_CLIENT_ID and WG_ACCESS_CLIENT_SECRET",
			"source", "the cell's .credentials/control-api.env")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	w := &watcher{town: t, cell: *cell, api: c, log: log, stall: *stall, dryRun: *dryRun}

	if *once {
		if err := w.pass(ctx, "manual"); err != nil {
			log.Error("pass failed", "error", err)
			os.Exit(1)
		}
		return
	}

	// Event-driven, with a slow sweep behind it. The tick only asks the
	// filesystem whether the event channel has changed; a tick that finds
	// nothing new and is not due a sweep does no work at all and, crucially,
	// makes no model call — which is the entire difference from the patrol it
	// replaces.
	lastCount, lastEvent := t.EventsFingerprint()
	lastSweep := time.Now()
	log.Info("witness started",
		"town", *town, "cell", *cell, "interval", interval.String(),
		"sweep", sweep.String(), "stall_after", stall.String(),
		"events_seen", lastCount)

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	// One pass at startup, because the town may have been down while something
	// went wrong and the events that would have woken us are already on disk.
	if err := w.pass(ctx, "startup"); err != nil {
		log.Error("startup pass failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			log.Info("stopping")
			return
		case <-ticker.C:
			count, newest := t.EventsFingerprint()
			trigger := ""
			switch {
			case count != lastCount || newest.After(lastEvent):
				trigger = "event"
			case time.Since(lastSweep) >= *sweep:
				trigger = "sweep"
			}
			lastCount, lastEvent = count, newest
			if trigger == "" {
				continue
			}
			lastSweep = time.Now()
			if err := w.pass(ctx, trigger); err != nil {
				log.Error("pass failed", "trigger", trigger, "error", err)
			}
		}
	}
}

type watcher struct {
	town   witness.Town
	cell   string
	api    *controlAPI
	log    *slog.Logger
	stall  time.Duration
	dryRun bool
}

// pass decides about every polecat in every rig, acts, and reports.
func (w *watcher) pass(ctx context.Context, trigger string) error {
	rigs, err := w.town.Rigs()
	if err != nil {
		return err
	}

	var tally witness.Tally
	for _, rig := range rigs {
		pcs, err := w.town.Polecats(ctx, rig.Name)
		if err != nil {
			// One rig failing must not take the others out. A rig whose Dolt
			// server is still starting returns an error for a minute or two
			// after the town comes up, and that is not a health finding.
			w.log.Warn("could not list polecats", "rig", rig.Name, "error", err)
			tally.Failed++
			continue
		}

		decisions := make([]witness.Decision, 0, len(pcs))
		now := time.Now()
		for _, p := range pcs {
			hb, err := w.town.Heartbeat(rig, p.Name)
			if err != nil {
				// An unreadable heartbeat is treated as no heartbeat. That is
				// the conservative reading: it can produce an "observe" where a
				// stall existed, but never an action taken on bad evidence.
				w.log.Warn("could not read a heartbeat",
					"rig", rig.Name, "polecat", p.Name, "error", err)
				hb = nil
			}
			d := witness.Decide(p, hb, now, w.stall)
			tally.Add(d.Action)

			if d.Action == witness.Nuke {
				if w.dryRun {
					w.log.Info("would nuke", "polecat", p.Ref(), "basis", d.Basis)
				} else if err := w.town.NukePolecat(ctx, p.Ref()); err != nil {
					w.log.Error("nuke failed", "polecat", p.Ref(), "error", err)
					tally.Failed++
					// Downgraded to an escalation. A nuke that will not run is
					// something a person should see, not something to retry
					// silently on every pass.
					d.Action = witness.Escalate
					d.Reason = "nuke-failed"
					d.Basis = d.Basis + "; gt polecat nuke failed: " + err.Error()
				} else {
					w.log.Info("nuked a completed polecat", "polecat", p.Ref(), "basis", d.Basis)
				}
			}
			decisions = append(decisions, d)
		}

		if len(decisions) == 0 {
			continue
		}
		report := witness.Report{Cell: w.cell, Repository: rig.Repository, Decisions: decisions}
		w.deliver(ctx, rig, report)
	}

	w.log.Info("pass complete", append([]any{"trigger", trigger, "rigs", len(rigs)}, tally.LogArgs()...)...)
	return nil
}

// deliver sends the report, and says plainly when it cannot.
func (w *watcher) deliver(ctx context.Context, rig witness.Rig, report witness.Report) {
	// Escalations are always logged locally, whether or not the control plane
	// is reachable. The node's journal is the fallback record; losing the
	// network must not lose the finding.
	for _, d := range report.Decisions {
		if d.Action == witness.Escalate || d.Action == witness.Ambiguous {
			w.log.Warn("escalating",
				"rig", d.Rig, "polecat", d.Polecat, "action", string(d.Action),
				"reason", d.Reason, "basis", d.Basis, "bead", d.Bead)
		}
	}

	if w.api.base == "" {
		return
	}
	if rig.Repository == "" {
		w.log.Warn("no repository for this rig, so escalations cannot be routed to a project",
			"rig", rig.Name)
		return
	}
	out, err := w.api.report(ctx, report)
	if err != nil {
		w.log.Error("could not report to the control plane", "rig", rig.Name, "error", err)
		return
	}
	if out.Unaddressed > 0 {
		// Nobody to tell is a failure, not a quiet success.
		w.log.Error("escalations reached nobody",
			"rig", rig.Name, "unaddressed", out.Unaddressed,
			"repository", rig.Repository,
			"hint", "the owning project needs a project_lead or backup_operator")
	}
	w.log.Info("reported",
		"rig", rig.Name, "recorded", out.Recorded,
		"escalated", out.Escalated, "notified", out.Notified)
}

type controlAPI struct {
	base   string
	token  string
	secret string
	client *http.Client
}

func (c *controlAPI) report(ctx context.Context, r witness.Report) (witness.Outcome, error) {
	var out witness.Outcome

	body, err := json.Marshal(r)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/v1/agent-health", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token == "" || c.secret == "" {
		return out, errors.New("WG_ACCESS_CLIENT_ID and WG_ACCESS_CLIENT_SECRET are not set; " +
			"the cell's .credentials/control-api.env supplies them")
	}
	// The cell's Cloudflare Access service token. Same credential the agents
	// use to mint git tokens, and bound by Access to this path.
	req.Header.Set("CF-Access-Client-Id", c.token)
	req.Header.Set("CF-Access-Client-Secret", c.secret)

	resp, err := c.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("control api returned %d: %s", resp.StatusCode, bytes.TrimSpace(payload))
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return out, fmt.Errorf("parsing the response: %w", err)
	}
	return out, nil
}
