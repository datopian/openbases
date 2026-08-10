// Package audit records immutable activity.
//
// Audit records are append-only and are exported to retention-locked storage.
// Secret values never appear in an audit record (plan sections 15.3, 17.2).
package audit

import (
	"context"
	"time"
)

// Record is one audited action.
type Record struct {
	At            time.Time
	ActorUserID   string
	ActorAgentID  string
	Action        string
	TargetType    string
	TargetID      string
	ProjectID     string
	Outcome       string // allowed | denied | executed | failed
	Reason        string
	CorrelationID string
	// EvidenceRefs point at pull requests, runs, digests, or approvals.
	EvidenceRefs []string
}

// Recorder appends audit records. An implementation must never drop a record
// silently; if it cannot append, the caller must fail the action.
type Recorder interface {
	Append(ctx context.Context, r Record) error
}
