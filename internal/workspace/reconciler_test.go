package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeStore records what the reconciler wrote.
type fakeStore struct {
	sources []Source
	subs    []Subscription
	loadErr error

	recorded map[string]GoogleSubscription
	failures map[string]string
	forgot   []string
}

func (f *fakeStore) Load(context.Context) ([]Source, []Subscription, error) {
	return f.sources, f.subs, f.loadErr
}

func (f *fakeStore) Record(_ context.Context, id string, g GoogleSubscription, failure error) error {
	if f.recorded == nil {
		f.recorded = map[string]GoogleSubscription{}
		f.failures = map[string]string{}
	}
	f.recorded[id] = g
	if failure != nil {
		f.failures[id] = failure.Error()
	}
	return nil
}

func (f *fakeStore) Forget(_ context.Context, id string) error {
	f.forgot = append(f.forgot, id)
	return nil
}

// fakeGoogle is the Workspace Events API, answering with operations exactly as
// the real one does.
type fakeGoogle struct {
	calls []string
	// failCreate makes create return an API error.
	failCreate bool
	// pending makes create return an unfinished operation, so the poll path runs.
	pending bool
	// nameless makes create succeed but return no subscription name.
	nameless bool
	polls    int
	// listed is what Google reports it holds.
	listed []map[string]any
	// listErr makes the inventory read fail.
	listErr bool
}

func (f *fakeGoogle) server(t *testing.T) *Events {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/subscriptions":
			if f.failCreate {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"error":{"code":403,"message":"caller lacks the drive.readonly scope"}}`)
				return
			}
			name := "subscriptions/new-1"
			if f.nameless {
				name = ""
			}
			if f.pending {
				json.NewEncoder(w).Encode(map[string]any{"name": "operations/op-1", "done": false})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"name": "operations/op-1", "done": true,
				"response": subJSON(name, "ACTIVE", "2026-09-30T00:00:00Z"),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/subscriptions":
			if f.listErr {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"error":{"code":403,"message":"no permission to list"}}`)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"subscriptions": f.listed})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/operations/"):
			f.polls++
			json.NewEncoder(w).Encode(map[string]any{
				"name": "operations/op-1", "done": true,
				"response": subJSON("subscriptions/new-1", "ACTIVE", "2026-09-30T00:00:00Z"),
			})
		case r.Method == http.MethodPatch:
			json.NewEncoder(w).Encode(map[string]any{
				"name": "operations/renew", "done": true,
				"response": subJSON(strings.TrimPrefix(r.URL.Path, "/"), "ACTIVE", "2026-09-28T00:00:00Z"),
			})
		case strings.HasSuffix(r.URL.Path, ":reactivate"):
			json.NewEncoder(w).Encode(map[string]any{
				"name": "operations/react", "done": true,
				"response": subJSON(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ":reactivate"),
					"ACTIVE", "2026-09-01T00:00:00Z"),
			})
		case r.Method == http.MethodDelete:
			json.NewEncoder(w).Encode(map[string]any{"name": "operations/del", "done": true, "response": map[string]any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":404,"message":"no such route in the fake"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Events{
		Token:   func(context.Context) (string, error) { return "test-token", nil },
		BaseURL: srv.URL,
		HTTP:    srv.Client(),
	}
}

// mutations drops the inventory read, which every pass makes.
func mutations(calls []string) []string {
	var out []string
	for _, c := range calls {
		if c != "GET /subscriptions" {
			out = append(out, c)
		}
	}
	return out
}

func subJSON(name, state, expiry string) map[string]any {
	return map[string]any{
		"name": name, "state": state, "expireTime": expiry,
		"eventTypes": driveTypes,
	}
}

func newReconciler(t *testing.T, st *fakeStore, g *fakeGoogle) *Reconciler {
	t.Helper()
	return &Reconciler{
		Store:  st,
		Events: g.server(t),
		Topic:  "projects/p/topics/t",
		Wants:  want,
		Policy: DefaultPolicy,
		Now:    func() time.Time { return now },
	}
}

// The failure this catches is the one the discovery document revealed: create,
// patch, reactivate and delete all return an Operation, not a Subscription.
// Unmarshalling the operation straight into a subscription stores
// "operations/<id>" as the subscription name and finds no state, so the next
// pass replaces a perfectly good subscription — forever, while reporting
// success every time.
func TestCreateStoresTheSubscriptionNameNotTheOperationName(t *testing.T) {
	st := &fakeStore{sources: []Source{src("a", true)}}
	rep, err := newReconciler(t, st, &fakeGoogle{}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 0 {
		t.Fatalf("failed: %v", rep.Err())
	}
	got := st.recorded["a"]
	if got.Name != "subscriptions/new-1" {
		t.Errorf("stored name = %q, want the subscription name", got.Name)
	}
	if got.LifecycleState() != StateActive {
		t.Errorf("stored state = %q, want active", got.LifecycleState())
	}
	if _, ok := got.Expiry(); !ok {
		t.Error("no expiry stored; without one the next pass replaces this subscription")
	}
}

// An operation that is not done yet has to be polled. Not polling means
// treating an in-flight create as having produced nothing.
func TestAnUnfinishedOperationIsPolled(t *testing.T) {
	st := &fakeStore{sources: []Source{src("a", true)}}
	g := &fakeGoogle{pending: true}
	if _, err := newReconciler(t, st, g).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.polls == 0 {
		t.Error("the operation was never polled")
	}
	if st.recorded["a"].Name != "subscriptions/new-1" {
		t.Errorf("stored %q after polling", st.recorded["a"].Name)
	}
}

// Idempotence, stated as a property: a pass over healthy subscriptions calls
// nothing. This is what makes running the timer more often harmless, and it is
// the acceptance criterion about no duplicate effects.
func TestAHealthyPassCallsNothing(t *testing.T) {
	st := &fakeStore{
		sources: []Source{src("a", true), src("b", true)},
		subs: []Subscription{
			sub("a", StateActive, 5*24*time.Hour),
			sub("b", StateActive, 5*24*time.Hour),
		},
	}
	g := &fakeGoogle{}
	rep, err := newReconciler(t, st, g).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := mutations(g.calls); len(got) != 0 {
		t.Errorf("changed something on a healthy pass: %v", got)
	}
	if len(g.calls) != 1 {
		t.Errorf("calls = %v; a pass reads Google's inventory exactly once", g.calls)
	}
	if rep.Changed != 0 || len(st.recorded) != 0 {
		t.Errorf("changed %d and wrote %d rows on a healthy pass", rep.Changed, len(st.recorded))
	}
}

// Replace must delete first. Creating first leaves two live subscriptions on
// one target for a moment, and both deliver.
func TestReplaceDeletesBeforeCreating(t *testing.T) {
	st := &fakeStore{
		sources: []Source{src("a", true)},
		subs:    []Subscription{sub("a", StateActive, -time.Hour)}, // expired
	}
	g := &fakeGoogle{}
	if _, err := newReconciler(t, st, g).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := mutations(g.calls)
	if len(calls) < 2 {
		t.Fatalf("calls = %v", calls)
	}
	if !strings.HasPrefix(calls[0], "DELETE") {
		t.Errorf("first call was %q; a replace that creates before deleting double-delivers", calls[0])
	}
	if !strings.HasPrefix(calls[1], "POST /subscriptions") {
		t.Errorf("second call was %q, want the create", calls[1])
	}
}

// Suspension is what a revoked permission looks like from Google's side.
// Reactivation keeps the id; the follow-up renewal is because reactivating does
// not extend an expiry that lapsed while it was suspended.
func TestReactivationIsFollowedByRenewal(t *testing.T) {
	st := &fakeStore{
		sources: []Source{src("a", true)},
		subs:    []Subscription{sub("a", StateSuspended, 5*24*time.Hour)},
	}
	g := &fakeGoogle{}
	if _, err := newReconciler(t, st, g).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(g.calls, " | ")
	if !strings.Contains(joined, ":reactivate") {
		t.Errorf("never reactivated: %v", g.calls)
	}
	if !strings.Contains(joined, "PATCH") {
		t.Errorf("reactivated but never renewed: %v", g.calls)
	}
	if st.recorded["a"].Name != "subscriptions/a" {
		t.Errorf("reactivation changed the id to %q; it must keep it", st.recorded["a"].Name)
	}
}

// A disabled source's subscription is deleted, not left to expire. Left alone
// it keeps delivering for days after someone revoked it, which is the
// acceptance criterion about a removed permission preventing new retrieval.
func TestADisabledSourceIsUnsubscribedImmediately(t *testing.T) {
	st := &fakeStore{
		sources: []Source{src("a", false)},
		subs:    []Subscription{sub("a", StateActive, 5*24*time.Hour)},
	}
	g := &fakeGoogle{}
	if _, err := newReconciler(t, st, g).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := mutations(g.calls); len(calls) != 1 || !strings.HasPrefix(calls[0], "DELETE") {
		t.Errorf("calls = %v, want a single delete", calls)
	}
	if len(st.forgot) != 1 || st.forgot[0] != "a" {
		t.Errorf("forgot = %v", st.forgot)
	}
}

// A subscription with no allow-listed source is deleted too. This is the
// direction a source-only walk cannot see.
func TestTheReconcilerDeletesASubscriptionWithNoSource(t *testing.T) {
	st := &fakeStore{
		sources: []Source{src("a", true)},
		subs: []Subscription{
			sub("a", StateActive, 5*24*time.Hour),
			sub("ghost", StateActive, 5*24*time.Hour),
		},
	}
	g := &fakeGoogle{}
	if _, err := newReconciler(t, st, g).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := mutations(g.calls); len(calls) != 1 || !strings.Contains(calls[0], "subscriptions/ghost") {
		t.Errorf("calls = %v, want only the ghost deleted", calls)
	}
}

// One broken source must not stop the others being renewed. Stopping at the
// first error means a single misconfigured source lets every other
// subscription expire over the following week.
func TestOneFailureDoesNotAbandonTheOtherSources(t *testing.T) {
	st := &fakeStore{
		sources: []Source{src("a", true), src("b", true)},
		subs:    []Subscription{sub("b", StateActive, time.Hour)}, // inside the renewal window
	}
	g := &fakeGoogle{failCreate: true}
	rep, err := newReconciler(t, st, g).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 1 {
		t.Errorf("failed = %d, want 1", rep.Failed)
	}
	if rep.Changed != 1 {
		t.Errorf("changed = %d; b's renewal should have gone through anyway", rep.Changed)
	}
	if _, ok := st.failures["a"]; !ok {
		t.Error("the failure was not recorded against the source, so nobody can see why it is broken")
	}
	if st.recorded["a"].LifecycleState() != StateFailed {
		t.Errorf("state = %q after a failed create, want failed", st.recorded["a"].LifecycleState())
	}
	if err := rep.Err(); err == nil || !strings.Contains(err.Error(), "drive.readonly") {
		t.Errorf("the report does not carry Google's reason: %v", err)
	}
}

// A create that returns no name is not a success: we would have no id to renew
// or delete, and the next pass would create a second subscription on the same
// target.
func TestACreateWithNoNameIsAFailure(t *testing.T) {
	st := &fakeStore{sources: []Source{src("a", true)}}
	rep, err := newReconciler(t, st, &fakeGoogle{nameless: true}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 1 {
		t.Errorf("failed = %d, want 1", rep.Failed)
	}
}

// An empty allow-list makes every subscription an orphan, and Reconcile would
// correctly delete all of them. The one plausible cause is RLS hiding the table
// or migrations not applied — so the pass refuses rather than destroying
// production.
func TestAnEmptyAllowListIsRefused(t *testing.T) {
	st := &fakeStore{subs: []Subscription{sub("a", StateActive, 5*24*time.Hour)}}
	g := &fakeGoogle{}
	if _, err := newReconciler(t, st, g).Run(context.Background()); err == nil {
		t.Fatal("an empty allow-list was accepted")
	}
	if len(g.calls) != 0 {
		t.Errorf("called %v before refusing", g.calls)
	}
}

// A dry run must work with no credential at all, or nobody will use it to check
// a pass before letting the timer make it.
func TestADryRunCallsNothingAndNeedsNoCredential(t *testing.T) {
	st := &fakeStore{sources: []Source{src("a", true)}}
	r := &Reconciler{Store: st, Wants: want, Policy: DefaultPolicy,
		Now: func() time.Time { return now }, DryRun: true}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Changed != 0 || len(st.recorded) != 0 {
		t.Error("a dry run wrote something")
	}
	if len(rep.Outcomes) != 1 || !rep.Outcomes[0].Skipped {
		t.Errorf("outcomes = %+v", rep.Outcomes)
	}
}

// Creating without a topic yields a subscription that delivers nowhere, which
// is indistinguishable from a source that never fires.
func TestATopiclessPassIsRefused(t *testing.T) {
	st := &fakeStore{sources: []Source{src("a", true)}}
	r := &Reconciler{Store: st, Wants: want, Policy: DefaultPolicy,
		Now: func() time.Time { return now }}
	if _, err := r.Run(context.Background()); err == nil {
		t.Fatal("a pass with no notification topic was accepted")
	}
}

// Drive requires includeDescendants and Meet must not carry driveOptions at
// all. Both were established against the live API; getting either wrong means
// the create is refused outright.
func TestTheCreateRequestMatchesWhatEachTargetAccepts(t *testing.T) {
	r := &Reconciler{Topic: "projects/p/topics/t", Wants: want}
	drive, err := r.request(Source{ID: "d", Kind: KindDrive, ExternalID: "0ABC"})
	if err != nil {
		t.Fatal(err)
	}
	if !drive.IncludeDescendants {
		t.Error("a shared-drive subscription without includeDescendants is refused")
	}
	if !strings.HasPrefix(drive.Target, "//drive.googleapis.com/drives/") {
		t.Errorf("drive target = %q", drive.Target)
	}
	meet, err := r.request(Source{ID: "m", Kind: KindMeet, ExternalID: "spaces/XYZ"})
	if err != nil {
		t.Fatal(err)
	}
	if meet.IncludeDescendants {
		t.Error("driveOptions on a Meet subscription is not a thing")
	}
	if meet.Target != "//meet.googleapis.com/spaces/XYZ" {
		t.Errorf("meet target = %q", meet.Target)
	}
	if sameTypes(meet.EventTypes, drive.EventTypes) {
		t.Error("Meet and Drive got the same event types; neither accepts the other's")
	}
	for _, e := range meet.EventTypes {
		if !strings.Contains(e, ".meet.") {
			t.Errorf("Meet subscription asked for %q", e)
		}
	}
}

// A kind with no configured event types is refused rather than treated as
// wanting nothing, which would replace the subscription on every pass.
func TestASourceKindWithNoEventTypesIsRefused(t *testing.T) {
	_, err := Reconcile([]Source{{ID: "m", Kind: KindMeet, Enabled: true}},
		nil, Wants{KindDrive: driveTypes}, now, DefaultPolicy)
	if err == nil {
		t.Fatal("a Meet source was reconciled against Drive-only event types")
	}
	if !strings.Contains(err.Error(), "meet") {
		t.Errorf("the error does not name the kind: %v", err)
	}
}

// liveSub is what Google reports holding, in the shape List returns.
func liveSub(name, target, topic string) map[string]any {
	return map[string]any{
		"name": name, "targetResource": target, "state": "ACTIVE",
		"expireTime": "2026-09-30T00:00:00Z", "eventTypes": driveTypes,
		"notificationEndpoint": map[string]string{"pubsubTopic": topic},
	}
}

// The failure this catches: a pass that created a subscription and crashed
// before writing it down. The source looks unsubscribed, so the next pass
// creates a second one on the same target and every event arrives twice.
func TestASubscriptionGoogleHoldsButWeLostIsAdoptedNotDuplicated(t *testing.T) {
	st := &fakeStore{sources: []Source{src("a", true)}}
	g := &fakeGoogle{listed: []map[string]any{
		liveSub("subscriptions/lost-1", "//drive.googleapis.com/drives/0ACa",
			"projects/p/topics/t"),
	}}
	rep, err := newReconciler(t, st, g).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range mutations(g.calls) {
		if strings.HasPrefix(c, "POST /subscriptions") {
			t.Errorf("created a second subscription on a target that already had one: %v", g.calls)
		}
	}
	if st.recorded["a"].Name != "subscriptions/lost-1" {
		t.Errorf("adopted name = %q", st.recorded["a"].Name)
	}
	if rep.Failed != 0 {
		t.Errorf("failed: %v", rep.Err())
	}
}

// A subscription pointing at something we do not allow-list is deleted.
// Ignoring its events while leaving it running means Google keeps sending them.
func TestASubscriptionOnANonAllowListedTargetIsDeleted(t *testing.T) {
	st := &fakeStore{
		sources: []Source{src("a", true)},
		subs:    []Subscription{sub("a", StateActive, 5*24*time.Hour)},
	}
	g := &fakeGoogle{listed: []map[string]any{
		liveSub("subscriptions/stray", "//drive.googleapis.com/drives/0ANOTOURS",
			"projects/p/topics/t"),
	}}
	if _, err := newReconciler(t, st, g).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := mutations(g.calls)
	if len(calls) != 1 || !strings.Contains(calls[0], "subscriptions/stray") {
		t.Errorf("calls = %v, want the stray deleted and nothing else", calls)
	}
}

// The service account is shared between environments, so a subscription
// notifying a different topic belongs to a different environment. Deleting it
// because we do not recognise it means staging deletes production's
// subscriptions — a blast radius created by tidiness.
func TestASubscriptionNotifyingAnotherTopicIsLeftAlone(t *testing.T) {
	st := &fakeStore{
		sources: []Source{src("a", true)},
		subs:    []Subscription{sub("a", StateActive, 5*24*time.Hour)},
	}
	g := &fakeGoogle{listed: []map[string]any{
		liveSub("subscriptions/production", "//drive.googleapis.com/drives/0ACa",
			"projects/p/topics/production"),
	}}
	if _, err := newReconciler(t, st, g).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := mutations(g.calls); len(calls) != 0 {
		t.Errorf("touched another environment's subscription: %v", calls)
	}
}

// Failing to list must not stop renewals. Refusing to act because we could not
// enumerate lets every subscription expire over the following days, which is a
// worse outcome than one unrecorded stray surviving another hour.
func TestAFailedInventoryReadStillRenews(t *testing.T) {
	st := &fakeStore{
		sources: []Source{src("a", true)},
		subs:    []Subscription{sub("a", StateActive, time.Hour)},
	}
	g := &fakeGoogle{listErr: true}
	rep, err := newReconciler(t, st, g).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Changed != 1 {
		t.Errorf("changed = %d, want the renewal to have gone through", rep.Changed)
	}
}

// subscriptions.list refuses an empty filter with INVALID_ARGUMENT: the
// discovery document marks it required and says at least one event type must be
// named. An empty one fails quietly — listing errors, adoption is skipped with a
// warning, and the subscription we lost track of is never found.
func TestTheInventoryReadNamesEventTypes(t *testing.T) {
	f := want.Filter()
	if !strings.Contains(f, `event_types:"`) {
		t.Fatalf("filter = %q, which the API rejects", f)
	}
	if strings.Count(f, "event_types:") != len(driveTypes)+len(meetTypes) {
		t.Errorf("filter = %q; every wanted type has to be named or its subscriptions are invisible", f)
	}
	if (Wants{}).Filter() != "" {
		t.Error("an empty Wants produced a filter")
	}
}
