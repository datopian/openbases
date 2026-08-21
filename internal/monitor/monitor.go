// Package monitor decides whether the platform is in trouble and who to tell.
//
// The threat model names detection as the weakest link, and it named it from
// experience: a database password was silently reset, reconciliation failed SASL
// authentication on every attempt, the backlog stopped draining, and nobody
// noticed until somebody asked why a view looked stale. Every individual piece
// was working as designed. What was missing was anything whose job was to look.
//
// The design follows the deterministic witness (ADR-0019) deliberately. The
// decision of whether something is wrong is a pure function of observed numbers,
// separated from both the gathering and the reporting, so the interesting cases
// — the ones that are painful to arrange against a live system — are ordinary
// table-driven tests. Collection needs a database, a filesystem and a socket.
// Judgement needs none of them.
//
// What this package deliberately does NOT do is decide anything by asking a
// model. An alerting path that costs money per evaluation is one somebody turns
// off, and a non-deterministic alerting path cannot be tested at all.
package monitor

import (
	"time"
)

// Class is one kind of trouble: its inbox rule, how loudly it ranks against
// everything else competing for an operator's attention, and the runbook that
// says what to do about it.
//
// The runbook is part of the class rather than something added at the reporting
// layer, because an alert without one is a notification that something is wrong
// and no help whatsoever at 3am. The acceptance criterion for this work package
// is that the alert reaches the operator WITH a runbook link, so the link is
// structural: there is no way to declare a class without one, and a test asserts
// every runbook file actually exists.
type Class struct {
	// Name is the short identifier used in logs, metrics and the acceptance
	// test.
	Name string
	// Rule is the inbox rule. Operators filter and unsubscribe by rule, so
	// each class is its own rule even where two classes share a cause.
	Rule string
	// Runbook is the repository-relative path to the procedure.
	Runbook string
	// Score orders this against every other inbox rule on the same 0..1 scale.
	Score float64
}

// DedupeKey is the identity of an ongoing problem.
//
// It is deliberately STABLE for the lifetime of a problem rather than including
// a timestamp. system_raise_attention upserts on (user_id, dedupe_key) for items
// that are open or snoozed: a stable key means one inbox item per operator that
// is refreshed with current detail on every cycle, and a snooze the operator set
// survives the machine noticing the problem again. A key containing the time
// would instead produce a new item every cycle, which is how an alerting system
// teaches people to ignore it.
func (c Class) DedupeKey() string { return "monitor:" + c.Name }

// The five classes this package covers, which are the five the work package
// requires: API, webhook, agent stall, disk and backup.
var (
	// ClassAPI is the control API being unable to serve traffic. Highest score
	// because everything else the platform does is reached through it.
	ClassAPI = Class{
		Name:    "api",
		Rule:    "platform_api_unavailable",
		Runbook: "docs/runbooks/api-unavailable.md",
		Score:   0.98,
	}

	// ClassWebhook is inbound GitHub deliveries arriving and not being
	// processed. This is the shape of the failure that actually happened: the
	// endpoint kept accepting deliveries and returning 200 while nothing
	// downstream consumed them, so from GitHub's side everything looked fine.
	ClassWebhook = Class{
		Name:    "webhook",
		Rule:    "platform_webhook_backlog",
		Runbook: "docs/runbooks/webhook-backlog.md",
		Score:   0.9,
	}

	// ClassAgentStall is the WITNESS having gone quiet, not an agent having
	// stalled.
	//
	// A stalled agent is already reported: the deterministic witness decides it
	// and raises agent_work_at_risk or agent_silent through the same inbox. This
	// class covers the failure that path cannot report, because it is the path —
	// if the witness dies, every future stall goes unnoticed and the inbox stays
	// reassuringly empty. Silence from a component whose job is to report
	// trouble is indistinguishable from good news, which is why it needs its own
	// alert rather than being inferred from the absence of one.
	ClassAgentStall = Class{
		Name:    "agent_stall",
		Rule:    "platform_agent_health_silent",
		Runbook: "docs/runbooks/agent-health-silent.md",
		Score:   0.85,
	}

	// ClassDisk is a filesystem approaching full. It ranks below the others
	// because it is a warning about the future rather than a present outage —
	// but only just, because PostgreSQL on a full disk stops accepting writes
	// and the recovery is materially worse than the prevention.
	ClassDisk = Class{
		Name:    "disk",
		Rule:    "platform_disk_pressure",
		Runbook: "docs/runbooks/disk-pressure.md",
		Score:   0.8,
	}

	// ClassBackup is a backup stream that is stale or has never run. Scored
	// like disk: nothing is broken right now, and everything is broken later.
	ClassBackup = Class{
		Name:    "backup",
		Rule:    "platform_backup_stale",
		Runbook: "docs/runbooks/backup-stale.md",
		Score:   0.8,
	}
)

// Classes is every class, in the order a report prints them.
var Classes = []Class{ClassAPI, ClassWebhook, ClassAgentStall, ClassDisk, ClassBackup}

// Finding is the verdict on one class.
//
// A finding is produced whether or not anything is wrong, because "checked and
// healthy" and "not checked" are different states and collapsing them is how a
// check silently stops running. Failing distinguishes them; Observed carries the
// numbers the verdict was reached from, so the inbox item explains itself rather
// than asserting a conclusion.
type Finding struct {
	Class    Class
	Failing  bool
	Summary  string
	Observed map[string]any
}

// Thresholds are the numbers that separate healthy from failing.
//
// They are configuration rather than constants because the right value differs
// between staging and production, and because a threshold that cannot be changed
// without a release is one that gets worked around instead.
type Thresholds struct {
	// WebhookBacklogAge is how long a delivery may sit unprocessed.
	//
	// It MUST be comfortably longer than the reconciliation period, because a
	// delivery that arrives just after a pass waits the full period by design.
	// Reconciliation runs every 15 minutes, and staging was observed with 20
	// deliveries whose oldest was 9 minutes — entirely healthy, and a 15-minute
	// threshold would have alerted on it within one cycle. An alerting system
	// that fires during correct operation is one people mute in week one.
	WebhookBacklogAge time.Duration
	// AgentHealthSilence is how long the control plane may receive no
	// agent-health report before the witness is presumed dead. Comfortably
	// more than the witness's own sweep interval, so a single missed pass is
	// not an alert.
	AgentHealthSilence time.Duration
	// DiskUsedPercent is the fullest a filesystem may get.
	DiskUsedPercent float64
}

// DefaultThresholds are the values the deployment uses unless overridden.
func DefaultThresholds() Thresholds {
	return Thresholds{
		// Three reconciliation periods. One missed pass is normal — a slow
		// GitHub call, a restart during deployment. Three consecutive misses
		// means the consumer is not running, which is the condition worth
		// waking somebody for.
		WebhookBacklogAge:  45 * time.Minute,
		AgentHealthSilence: 30 * time.Minute,
		DiskUsedPercent:    85,
	}
}
