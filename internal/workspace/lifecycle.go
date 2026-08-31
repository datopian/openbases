// Package workspace decides what to do about Google Workspace event
// subscriptions (WP-H1).
//
// Deciding is separated from doing for the reason the runner, the witness and
// the cost mapping are: every decision here has an expensive wrong answer and
// each is trivial as a table-driven test, while arranging the same case against
// a live tenant means waiting for a subscription to expire.
//
// The expensive wrong answers, specifically. Renewing too late loses events
// silently — an expired subscription stops delivering and says nothing.
// Renewing too eagerly burns quota and mints churn. Treating a source we no
// longer allow as merely "not due for renewal" leaves it delivering. And
// deciding a subscription is missing when it is only unread deletes one that
// works.
package workspace

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Visibility is what artefacts derived from a source inherit (ADR-0013).
type Visibility string

const (
	Internal     Visibility = "internal"
	Confidential Visibility = "confidential"
	Restricted   Visibility = "restricted"
)

// Kind is which Google product a source belongs to.
type Kind string

const (
	KindDrive Kind = "drive"
	KindMeet  Kind = "meet"
)

// Source is an allow-listed thing we may ingest from.
type Source struct {
	ID         string
	Kind       Kind
	ExternalID string
	Name       string
	Visibility Visibility
	Enabled    bool
}

// TargetResource is what Google's subscription API takes.
//
// Built rather than stored, so a source cannot carry a hand-typed resource name
// that disagrees with its own id.
func (s Source) TargetResource() (string, error) {
	id := strings.TrimSpace(s.ExternalID)
	if id == "" {
		return "", fmt.Errorf("source %q has no external id", s.Name)
	}
	switch s.Kind {
	case KindDrive:
		return "//drive.googleapis.com/drives/" + id, nil
	case KindMeet:
		// spaces/ is part of the resource name, and the stable server-generated
		// id belongs here rather than the typeable meeting code. The code is an
		// alias — tfy-qcsa-twb resolves to spaces/FS4Sj-9MIY0B — and an alias is
		// the same trap as matching a shared drive by name.
		return "//meet.googleapis.com/spaces/" + strings.TrimPrefix(id, "spaces/"), nil
	default:
		return "", fmt.Errorf("source %q has unknown kind %q", s.Name, s.Kind)
	}
}

// State is Google's view of a subscription.
type State string

const (
	StatePending   State = "pending"
	StateActive    State = "active"
	StateSuspended State = "suspended"
	StateDeleted   State = "deleted"
	StateFailed    State = "failed"
)

// Subscription is what we believe Google is holding for a source.
type Subscription struct {
	SourceID   string
	GoogleName string
	State      State
	ExpiresAt  time.Time
	EventTypes []string
}

// Action is what reconciliation decided to do about one source.
type Action string

const (
	// ActionNone leaves a healthy subscription alone.
	ActionNone Action = "none"
	// ActionCreate makes one that does not exist.
	ActionCreate Action = "create"
	// ActionRenew extends one before it expires.
	ActionRenew Action = "renew"
	// ActionReactivate is Google's own verb for a suspended subscription. It is
	// NOT the same as create: reactivating keeps the subscription id, so events
	// that arrived during the suspension are not re-delivered under a new one.
	ActionReactivate Action = "reactivate"
	// ActionUpdate changes what a subscription listens for, in place. The patch
	// method accepts event_types, so the id survives and no window exists with
	// no subscription — which a replacement would leave, losing every event
	// that arrived inside it.
	ActionUpdate Action = "update"
	// ActionReplace deletes and recreates. Needed when what we want cannot be
	// reached by patching at all — a subscription Google has deleted, one that
	// failed, or one whose expiry we cannot reason about.
	ActionReplace Action = "replace"
	// ActionDelete removes a subscription for a source we no longer allow.
	ActionDelete Action = "delete"
)

// Decision pairs an action with the reason for it.
//
// The reason is not decoration. Reconciliation runs unattended and its output is
// read after something has gone wrong, when "renew" alone does not say whether
// the subscription was nearly expired or the event types had drifted.
type Decision struct {
	SourceID string
	Action   Action
	Reason   string
}

// Policy is the timing reconciliation works to.
type Policy struct {
	// RenewBefore is how long ahead of expiry to renew.
	//
	// It must exceed the reconciliation interval, or a subscription can expire
	// between two runs that both considered it healthy. Validate enforces that
	// rather than leaving it to whoever sets the two numbers.
	RenewBefore time.Duration
	// Interval is how often reconciliation runs.
	Interval time.Duration
}

// DefaultPolicy renews a day ahead, reconciling hourly.
//
// A Workspace Events subscription lasts at most seven days, so a day is roughly
// a seventh of the life — enough to survive a night of failed runs without
// renewing on every pass.
var DefaultPolicy = Policy{RenewBefore: 24 * time.Hour, Interval: time.Hour}

// Validate refuses a policy that would lose events.
func (p Policy) Validate() error {
	if p.Interval <= 0 {
		return fmt.Errorf("reconciliation interval must be positive")
	}
	if p.RenewBefore <= 0 {
		return fmt.Errorf("renewal window must be positive")
	}
	if p.RenewBefore <= p.Interval {
		// The failure this prevents: a subscription expiring between two runs
		// that each saw it as healthy, which presents as a source that quietly
		// stops delivering.
		return fmt.Errorf("renewal window %s must exceed the reconciliation interval %s, "+
			"or a subscription can expire between two runs that both saw it as healthy",
			p.RenewBefore, p.Interval)
	}
	return nil
}

// Wants is the event types we subscribe to, per source kind.
//
// Per kind rather than one set for everything, because Drive and Meet accept
// disjoint event types and neither accepts the other's. A single set would make
// every Meet subscription look drifted from what we want, and drift is repaired
// by replacement — so the reconciler would delete and recreate every Meet
// subscription on every pass, forever, while reporting success.
type Wants map[Kind][]string

// For reports the event types wanted for a kind.
func (w Wants) For(k Kind) ([]string, error) {
	types, ok := w[k]
	if !ok || len(types) == 0 {
		// Refused rather than treated as "want nothing". An empty set compares
		// unequal to whatever the subscription actually has, so the source
		// would be replaced on every pass; and a subscription created with no
		// event types is rejected by Google anyway, so the loop would never
		// even converge on something broken.
		return nil, fmt.Errorf("no event types are configured for %s sources", k)
	}
	return types, nil
}

// Filter is the query subscriptions.list requires.
//
// The filter is not optional and must name at least one event type — the
// discovery document says "Required" and the server answers an empty filter
// with INVALID_ARGUMENT. Getting this wrong is quiet: listing fails, adoption
// is skipped with a warning, and the reconciler never notices the subscription
// it lost track of.
//
// One consequence worth naming: a stray subscription created with event types
// we no longer ask for cannot be enumerated at all, because every query has to
// name the types it wants. Such a stray is found only by its target coming up
// in a create that Google refuses as a duplicate.
func (w Wants) Filter() string {
	seen := map[string]bool{}
	var terms []string
	for _, types := range w {
		for _, t := range types {
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			terms = append(terms, `event_types:"`+t+`"`)
		}
	}
	sort.Strings(terms)
	return strings.Join(terms, " OR ")
}

// Reconcile decides what to do about every source and every subscription.
//
// Both directions matter. A source with no subscription needs one; a
// subscription whose source is gone or disabled needs removing. Walking only
// the sources would leave the second running forever, which is the shape of
// "a removed permission prevents new retrieval" failing.
func Reconcile(sources []Source, subs []Subscription, want Wants, now time.Time, p Policy) ([]Decision, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}

	bySource := make(map[string]Subscription, len(subs))
	for _, s := range subs {
		bySource[s.SourceID] = s
	}
	known := make(map[string]bool, len(sources))

	var out []Decision
	for _, src := range sources {
		known[src.ID] = true
		sub, exists := bySource[src.ID]

		wantTypes, err := want.For(src.Kind)
		if err != nil {
			// Not skipped. A source we cannot decide about is a source that
			// silently stops being reconciled, which is the failure this whole
			// package exists to make impossible.
			return nil, fmt.Errorf("source %s (%s): %w", src.ID, src.Name, err)
		}

		if !src.Enabled {
			if exists && sub.State != StateDeleted {
				out = append(out, Decision{src.ID, ActionDelete,
					"the source is disabled and its subscription is still " + string(sub.State)})
			}
			continue
		}

		switch {
		case !exists, sub.GoogleName == "":
			out = append(out, Decision{src.ID, ActionCreate, "no subscription exists for an enabled source"})

		case sub.State == StateDeleted, sub.State == StateFailed:
			// Google will not renew what it has deleted, and a failed
			// subscription does not recover by being asked again.
			out = append(out, Decision{src.ID, ActionReplace,
				"the subscription is " + string(sub.State) + " and cannot be renewed"})

		case sub.State == StateSuspended:
			out = append(out, Decision{src.ID, ActionReactivate,
				"the subscription is suspended and reactivating keeps its id"})

		case !sameTypes(sub.EventTypes, wantTypes):
			// Updated in place, not replaced. The patch method accepts
			// event_types, so the id survives and there is no window with no
			// subscription — a replacement would lose every event that arrived
			// between the delete and the create. Renewing alone would not do:
			// it extends a subscription without changing what it listens for,
			// so the new types would never arrive, silently, because the old
			// ones keep working.
			out = append(out, Decision{src.ID, ActionUpdate,
				"the event types have drifted from what we now subscribe to"})

		case sub.ExpiresAt.IsZero():
			// Active with no expiry is not a subscription we can reason about.
			// Treating it as healthy means never renewing it.
			out = append(out, Decision{src.ID, ActionReplace,
				"the subscription is active with no recorded expiry"})

		case !sub.ExpiresAt.After(now):
			out = append(out, Decision{src.ID, ActionReplace,
				"the subscription has already expired"})

		case sub.ExpiresAt.Sub(now) <= p.RenewBefore:
			out = append(out, Decision{src.ID, ActionRenew,
				fmt.Sprintf("expires in %s, inside the %s renewal window",
					sub.ExpiresAt.Sub(now).Round(time.Minute), p.RenewBefore)})

		default:
			out = append(out, Decision{src.ID, ActionNone,
				fmt.Sprintf("healthy, expires in %s", sub.ExpiresAt.Sub(now).Round(time.Minute))})
		}
	}

	// The other direction: a live subscription whose source is no longer
	// allow-listed at all.
	for _, sub := range subs {
		if known[sub.SourceID] || sub.State == StateDeleted {
			continue
		}
		out = append(out, Decision{sub.SourceID, ActionDelete,
			"the subscription has no allow-listed source"})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].SourceID < out[j].SourceID })
	return out, nil
}

// sameTypes compares two event-type sets ignoring order and duplicates.
func sameTypes(a, b []string) bool {
	norm := func(in []string) []string {
		seen := map[string]bool{}
		for _, s := range in {
			if s = strings.TrimSpace(s); s != "" {
				seen[s] = true
			}
		}
		out := make([]string, 0, len(seen))
		for s := range seen {
			out = append(out, s)
		}
		sort.Strings(out)
		return out
	}
	x, y := norm(a), norm(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
