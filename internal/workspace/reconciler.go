package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Store is what the reconciler needs from the database.
//
// An interface so the executor's tests — which are about calling Google exactly
// once and recording the result even when it fails — run without a database.
// DB is the implementation; nothing else implements it in production.
type Store interface {
	Load(ctx context.Context) ([]Source, []Subscription, error)
	Record(ctx context.Context, sourceID string, g GoogleSubscription, failure error) error
	Forget(ctx context.Context, sourceID string) error
}

// Reconciler turns decisions into Google API calls and writes back what
// happened.
//
// Split from Reconcile deliberately: Reconcile decides and is pure, this
// executes and is not. The tests that matter about *what* to do run without a
// network or a database; the tests here are about doing it exactly once and
// recording it even when it fails.
type Reconciler struct {
	Store  Store
	Events *Events
	// Topic is the Pub/Sub topic every subscription notifies.
	Topic  string
	Wants  Wants
	Policy Policy
	Log    *slog.Logger
	// Now is overridable for tests.
	Now func() time.Time
	// DryRun decides but calls nothing and writes nothing.
	DryRun bool
}

// Outcome is what happened to one decision.
type Outcome struct {
	Decision Decision
	Source   string // display name, for humans reading the log
	Err      error
	Skipped  bool // dry run
}

// Report is one reconciliation pass.
type Report struct {
	Outcomes []Outcome
	Changed  int
	Failed   int
}

// Err reports the pass as failed if any decision failed.
//
// Joined rather than first-only: with four sources, one broken delegation must
// not hide a second unrelated failure behind it.
func (r Report) Err() error {
	var errs []error
	for _, o := range r.Outcomes {
		if o.Err != nil {
			errs = append(errs, fmt.Errorf("%s: %s: %w", o.Source, o.Decision.Action, o.Err))
		}
	}
	return errors.Join(errs...)
}

// Run makes one pass.
//
// A failure on one source does not abandon the others. The alternative — stop
// at the first error — means a single misconfigured source stops every other
// subscription from being renewed, and they expire one by one over the
// following week.
func (r *Reconciler) Run(ctx context.Context) (Report, error) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	if r.Topic == "" && !r.DryRun {
		// Checked before deciding, not on the first create. Google accepts a
		// subscription with no notificationEndpoint in some shapes and then
		// delivers nowhere, which looks identical to a source that never fires.
		return Report{}, fmt.Errorf("a notification topic is required to create subscriptions")
	}

	sources, subs, err := r.Store.Load(ctx)
	if err != nil {
		return Report{}, err
	}
	if len(sources) == 0 {
		// Refused rather than proceeding. An empty allow-list makes every
		// subscription an orphan, and Reconcile would correctly decide to
		// delete all of them — so the one plausible cause (RLS hiding the
		// table, migrations not applied) would destroy production rather than
		// erroring.
		return Report{}, fmt.Errorf("no event sources are configured; refusing to treat every " +
			"subscription as an orphan (check that migrations 0043 and 0045 are applied)")
	}

	names := make(map[string]string, len(sources))
	byID := make(map[string]Source, len(sources))
	for _, s := range sources {
		names[s.ID] = s.Name
		byID[s.ID] = s
	}

	// What Google actually holds, before deciding anything.
	//
	// Without this step a subscription we lost track of — created by a pass that
	// crashed after the API call and before the write — is invisible: the source
	// looks unsubscribed, so the next pass creates a SECOND subscription on the
	// same target and every event arrives twice, which is precisely the
	// duplicate effect the acceptance criterion forbids.
	if !r.DryRun {
		subs, err = r.adopt(ctx, sources, subs)
		if err != nil {
			return Report{}, err
		}
	}

	decisions, err := Reconcile(sources, subs, r.Wants, now(), r.Policy)
	if err != nil {
		return Report{}, err
	}

	byName := make(map[string]Subscription, len(subs))
	for _, s := range subs {
		byName[s.SourceID] = s
	}

	rep := Report{}
	for _, d := range decisions {
		name := names[d.SourceID]
		if name == "" {
			name = "(no longer allow-listed) " + d.SourceID
		}
		out := Outcome{Decision: d, Source: name}

		if d.Action == ActionNone {
			r.logf(ctx, slog.LevelDebug, "left alone", d, name)
			rep.Outcomes = append(rep.Outcomes, out)
			continue
		}
		if r.DryRun {
			out.Skipped = true
			r.logf(ctx, slog.LevelInfo, "would act", d, name)
			rep.Outcomes = append(rep.Outcomes, out)
			continue
		}

		out.Err = r.apply(ctx, d, byID[d.SourceID], byName[d.SourceID])
		if out.Err != nil {
			rep.Failed++
			r.logf(ctx, slog.LevelError, "failed", d, name, "error", out.Err)
		} else {
			rep.Changed++
			r.logf(ctx, slog.LevelInfo, "applied", d, name)
		}
		rep.Outcomes = append(rep.Outcomes, out)
	}
	return rep, nil
}

// adopt reconciles our record against the subscriptions Google actually holds.
//
// Three outcomes per live subscription:
//
//   - We already know it: nothing to do, the source walk handles it.
//   - It targets an allow-listed source we have no record of: adopted, because
//     creating a second subscription on the same target double-delivers.
//   - It targets nothing we allow-list: deleted, because nothing renews it and
//     nobody is reading what it sends. This is the enforcement half of "a
//     non-allow-listed source is ignored" — ignoring the events while leaving
//     the subscription running means Google keeps sending them and the topic
//     keeps paying for them.
func (r *Reconciler) adopt(ctx context.Context, sources []Source, subs []Subscription) ([]Subscription, error) {
	filter := r.Wants.Filter()
	if filter == "" {
		return nil, fmt.Errorf("no event types are configured at all, so there is nothing to reconcile")
	}
	live, err := r.Events.List(ctx, filter)
	if err != nil {
		// Not fatal to the pass. Renewing the subscriptions we do know about is
		// more valuable than refusing to act because we could not enumerate;
		// the cost of skipping is that an unrecorded subscription survives one
		// more hour.
		if r.Log != nil {
			r.Log.Warn("could not list subscriptions; skipping adoption",
				"error", err)
		}
		return subs, nil
	}

	byTarget := make(map[string]Source, len(sources))
	for _, src := range sources {
		if !src.Enabled {
			continue
		}
		target, err := src.TargetResource()
		if err != nil {
			return nil, fmt.Errorf("source %s (%s): %w", src.ID, src.Name, err)
		}
		byTarget[target] = src
	}
	knownName := make(map[string]bool, len(subs))
	haveSub := make(map[string]bool, len(subs))
	for _, sub := range subs {
		knownName[sub.GoogleName] = true
		haveSub[sub.SourceID] = true
	}

	for _, g := range live {
		if g.Name == "" || knownName[g.Name] {
			continue
		}
		// Only subscriptions pointing at OUR topic. The service account is
		// shared between environments, so deleting by "we do not recognise it"
		// alone would have staging delete production's subscriptions — a
		// blast radius created by tidiness.
		if g.NotificationEndpoint == nil || g.NotificationEndpoint.PubsubTopic != r.Topic {
			continue
		}
		if g.LifecycleState() == StateDeleted {
			continue
		}

		src, allowed := byTarget[g.TargetResource]
		switch {
		case allowed && !haveSub[src.ID]:
			if err := r.Store.Record(ctx, src.ID, g, nil); err != nil {
				return nil, fmt.Errorf("adopting %s for %s: %w", g.Name, src.Name, err)
			}
			sub := Subscription{SourceID: src.ID, GoogleName: g.Name,
				State: g.LifecycleState(), EventTypes: g.EventTypes}
			if t, ok := g.Expiry(); ok {
				sub.ExpiresAt = t
			}
			subs = append(subs, sub)
			haveSub[src.ID] = true
			knownName[g.Name] = true
			if r.Log != nil {
				r.Log.Warn("adopted a subscription we had no record of",
					"source", src.Name, "subscription", g.Name,
					"note", "a previous pass created this and did not write it down")
			}

		default:
			// Either the target is not allow-listed, or its source already has a
			// recorded subscription and this is a duplicate on the same target.
			reason := "the target is not an allow-listed source"
			if allowed {
				reason = "a second subscription on a target that already has one"
			}
			if err := r.Events.Delete(ctx, g.Name); err != nil {
				// Logged, not fatal: an undeletable stray must not stop the
				// sources we can still renew.
				if r.Log != nil {
					r.Log.Error("could not delete a stray subscription",
						"subscription", g.Name, "target", g.TargetResource,
						"reason", reason, "error", err)
				}
				continue
			}
			if r.Log != nil {
				r.Log.Warn("deleted a stray subscription",
					"subscription", g.Name, "target", g.TargetResource, "reason", reason)
			}
		}
	}
	return subs, nil
}

func (r *Reconciler) logf(ctx context.Context, lvl slog.Level, msg string, d Decision, name string, extra ...any) {
	if r.Log == nil {
		return
	}
	args := append([]any{"source", name, "action", string(d.Action), "reason", d.Reason}, extra...)
	r.Log.Log(ctx, lvl, msg, args...)
}

// apply performs one decision and records the result.
//
// Recording happens on failure too. A create that failed leaves last_error set
// and the state failed, so the next pass replaces it and a human can see why —
// as opposed to leaving the row untouched, which makes a permanently broken
// source indistinguishable from one nobody has got to yet.
func (r *Reconciler) apply(ctx context.Context, d Decision, src Source, existing Subscription) error {
	switch d.Action {
	case ActionDelete:
		if existing.GoogleName != "" {
			if err := r.Events.Delete(ctx, existing.GoogleName); err != nil {
				// Not recorded as failed-with-state, because a delete that
				// failed must stay deletable: leaving google_name in place is
				// what lets the next pass try again.
				return fmt.Errorf("deleting %s: %w", existing.GoogleName, err)
			}
		}
		return r.Store.Forget(ctx, d.SourceID)

	case ActionRenew:
		g, err := r.Events.Renew(ctx, existing.GoogleName)
		return r.record(ctx, d.SourceID, g, existing, err)

	case ActionReactivate:
		g, err := r.Events.Reactivate(ctx, existing.GoogleName)
		if err == nil && g.LifecycleState() == StateActive {
			// Reactivation revives a subscription but does not extend it. One
			// that was suspended for a week comes back already inside the
			// renewal window, and waiting for the next pass to notice wastes a
			// whole interval of deliveries.
			if renewed, rerr := r.Events.Renew(ctx, existing.GoogleName); rerr == nil {
				g = renewed
			}
		}
		return r.record(ctx, d.SourceID, g, existing, err)

	case ActionCreate, ActionReplace:
		// Replace is delete-then-create, in that order. The reverse would leave
		// two live subscriptions on the same target for a moment, and both
		// would deliver — duplicate effects being exactly what the acceptance
		// criterion forbids.
		if d.Action == ActionReplace && existing.GoogleName != "" {
			if err := r.Events.Delete(ctx, existing.GoogleName); err != nil {
				return fmt.Errorf("deleting %s before replacing it: %w", existing.GoogleName, err)
			}
		}
		req, err := r.request(src)
		if err != nil {
			return r.record(ctx, d.SourceID, GoogleSubscription{}, Subscription{}, err)
		}
		g, err := r.Events.Create(ctx, req, false)
		if err == nil && g.Name == "" {
			// A create that returned no subscription name is not a success we
			// can record: we would have no id to renew or delete, and the next
			// pass would create a second one against the same target.
			err = fmt.Errorf("the subscription was created but Google returned no name")
		}
		return r.record(ctx, d.SourceID, g, Subscription{}, err)

	default:
		return fmt.Errorf("unhandled action %q", d.Action)
	}
}

// record writes the result, preferring what Google said over what we asked for.
func (r *Reconciler) record(ctx context.Context, sourceID string, g GoogleSubscription, existing Subscription, failure error) error {
	if failure != nil {
		// The name and types we already had are preserved so that a failed
		// renewal is still a subscription we know how to delete.
		g.Name = firstNonEmpty(g.Name, existing.GoogleName)
		if len(g.EventTypes) == 0 {
			g.EventTypes = existing.EventTypes
		}
		if err := r.Store.Record(ctx, sourceID, g, failure); err != nil {
			return errors.Join(failure, fmt.Errorf("recording the failure: %w", err))
		}
		return failure
	}
	return r.Store.Record(ctx, sourceID, g, nil)
}

// request builds the create call for a source.
func (r *Reconciler) request(src Source) (CreateRequest, error) {
	target, err := src.TargetResource()
	if err != nil {
		return CreateRequest{}, err
	}
	types, err := r.Wants.For(src.Kind)
	if err != nil {
		return CreateRequest{}, err
	}
	return CreateRequest{
		Target:     target,
		EventTypes: types,
		Topic:      r.Topic,
		// Required and immutable for a shared drive; meaningless for Meet.
		IncludeDescendants: src.Kind == KindDrive,
	}, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
