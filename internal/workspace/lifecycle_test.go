package workspace

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

var driveTypes = []string{
	"google.workspace.drive.file.v3.created",
	"google.workspace.drive.file.v3.trashed",
}

var meetTypes = []string{
	"google.workspace.meet.conference.v2.ended",
	"google.workspace.meet.transcript.v2.ended",
}

var want = Wants{KindDrive: driveTypes, KindMeet: meetTypes}

func src(id string, enabled bool) Source {
	return Source{ID: id, Kind: KindDrive, ExternalID: "0AC" + id, Name: id,
		Visibility: Internal, Enabled: enabled}
}

func sub(id string, state State, expiresIn time.Duration, types ...string) Subscription {
	s := Subscription{SourceID: id, GoogleName: "subscriptions/" + id, State: state,
		EventTypes: types}
	if expiresIn != 0 {
		s.ExpiresAt = now.Add(expiresIn)
	}
	if len(types) == 0 {
		s.EventTypes = driveTypes
	}
	return s
}

func only(t *testing.T, ds []Decision, sourceID string) Decision {
	t.Helper()
	for _, d := range ds {
		if d.SourceID == sourceID {
			return d
		}
	}
	t.Fatalf("no decision for %q in %v", sourceID, ds)
	return Decision{}
}

// The renewal window must exceed the reconciliation interval. Otherwise a
// subscription expires between two runs that each saw it as healthy, and the
// source stops delivering with nothing to notice.
func TestAPolicyThatCanLoseEventsIsRefused(t *testing.T) {
	for name, p := range map[string]Policy{
		"window equal to the interval": {RenewBefore: time.Hour, Interval: time.Hour},
		"window shorter than interval": {RenewBefore: 30 * time.Minute, Interval: time.Hour},
		"no interval":                  {RenewBefore: time.Hour},
		"no window":                    {Interval: time.Hour},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := DefaultPolicy.Validate(); err != nil {
		t.Errorf("the default policy is invalid: %v", err)
	}
}

func TestAHealthySubscriptionIsLeftAlone(t *testing.T) {
	ds, err := Reconcile([]Source{src("a", true)},
		[]Subscription{sub("a", StateActive, 5*24*time.Hour)}, want, now, DefaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if got := only(t, ds, "a").Action; got != ActionNone {
		t.Errorf("action = %q, want none", got)
	}
}

func TestASubscriptionInsideTheWindowIsRenewed(t *testing.T) {
	ds, _ := Reconcile([]Source{src("a", true)},
		[]Subscription{sub("a", StateActive, 6*time.Hour)}, want, now, DefaultPolicy)
	d := only(t, ds, "a")
	if d.Action != ActionRenew {
		t.Fatalf("action = %q, want renew", d.Action)
	}
	// The reason is read after something has gone wrong, so it has to say which
	// of several renewals this was.
	if !strings.Contains(d.Reason, "renewal window") {
		t.Errorf("reason does not explain the renewal: %q", d.Reason)
	}
}

func TestAnEnabledSourceWithNoSubscriptionGetsOne(t *testing.T) {
	ds, _ := Reconcile([]Source{src("a", true)}, nil, want, now, DefaultPolicy)
	if got := only(t, ds, "a").Action; got != ActionCreate {
		t.Errorf("action = %q, want create", got)
	}
}

// Renewing extends a subscription; it does not change what it listens for. A
// drifted set has to be replaced, or the new event types never arrive — and
// they never arrive SILENTLY, because the old ones keep working.
func TestDriftedEventTypesForceAReplacement(t *testing.T) {
	s := sub("a", StateActive, 5*24*time.Hour, "google.workspace.drive.file.v3.created")
	ds, _ := Reconcile([]Source{src("a", true)}, []Subscription{s}, want, now, DefaultPolicy)
	d := only(t, ds, "a")
	if d.Action != ActionReplace {
		t.Fatalf("action = %q, want replace", d.Action)
	}
	if !strings.Contains(d.Reason, "drift") {
		t.Errorf("reason = %q", d.Reason)
	}
}

// Order and duplicates are not drift. Treating them as drift would replace a
// working subscription on every run.
func TestOrderAndDuplicatesAreNotDrift(t *testing.T) {
	reordered := []string{driveTypes[1], driveTypes[0], driveTypes[0]}
	ds, _ := Reconcile([]Source{src("a", true)},
		[]Subscription{sub("a", StateActive, 5*24*time.Hour, reordered...)}, want, now, DefaultPolicy)
	if got := only(t, ds, "a").Action; got != ActionNone {
		t.Errorf("action = %q, want none — reordering is not drift", got)
	}
}

// Reactivate, not create. Reactivating keeps the subscription id, so events
// that arrived during the suspension are not re-delivered under a new one.
func TestASuspendedSubscriptionIsReactivated(t *testing.T) {
	ds, _ := Reconcile([]Source{src("a", true)},
		[]Subscription{sub("a", StateSuspended, 5*24*time.Hour)}, want, now, DefaultPolicy)
	if got := only(t, ds, "a").Action; got != ActionReactivate {
		t.Errorf("action = %q, want reactivate", got)
	}
}

func TestDeletedAndFailedSubscriptionsAreReplaced(t *testing.T) {
	for _, st := range []State{StateDeleted, StateFailed} {
		ds, _ := Reconcile([]Source{src("a", true)},
			[]Subscription{sub("a", st, 5*24*time.Hour)}, want, now, DefaultPolicy)
		if got := only(t, ds, "a").Action; got != ActionReplace {
			t.Errorf("state %q gave action %q, want replace", st, got)
		}
	}
}

// An active subscription with no expiry cannot be reasoned about. Calling it
// healthy means never renewing it.
func TestAnActiveSubscriptionWithNoExpiryIsReplaced(t *testing.T) {
	ds, _ := Reconcile([]Source{src("a", true)},
		[]Subscription{sub("a", StateActive, 0)}, want, now, DefaultPolicy)
	if got := only(t, ds, "a").Action; got != ActionReplace {
		t.Errorf("action = %q, want replace", got)
	}
}

func TestAnExpiredSubscriptionIsReplacedNotRenewed(t *testing.T) {
	ds, _ := Reconcile([]Source{src("a", true)},
		[]Subscription{sub("a", StateActive, -time.Hour)}, want, now, DefaultPolicy)
	if got := only(t, ds, "a").Action; got != ActionReplace {
		t.Errorf("action = %q, want replace", got)
	}
}

// "A removed user or provider permission prevents new retrieval" — a disabled
// source must stop delivering, not merely stop being renewed.
func TestADisabledSourceHasItsSubscriptionDeleted(t *testing.T) {
	ds, _ := Reconcile([]Source{src("a", false)},
		[]Subscription{sub("a", StateActive, 5*24*time.Hour)}, want, now, DefaultPolicy)
	if got := only(t, ds, "a").Action; got != ActionDelete {
		t.Errorf("action = %q, want delete", got)
	}
}

// Reconciliation walks both directions. A subscription whose source is no
// longer allow-listed at all would otherwise deliver forever.
func TestASubscriptionWithNoSourceIsDeleted(t *testing.T) {
	ds, _ := Reconcile(nil, []Subscription{sub("ghost", StateActive, 5*24*time.Hour)},
		want, now, DefaultPolicy)
	d := only(t, ds, "ghost")
	if d.Action != ActionDelete {
		t.Fatalf("action = %q, want delete", d.Action)
	}
	if !strings.Contains(d.Reason, "no allow-listed source") {
		t.Errorf("reason = %q", d.Reason)
	}
}

// Nothing to do for a disabled source that already has no subscription: the
// desired state is the actual state.
func TestADisabledSourceWithNoSubscriptionIsQuiet(t *testing.T) {
	ds, _ := Reconcile([]Source{src("a", false)}, nil, want, now, DefaultPolicy)
	if len(ds) != 0 {
		t.Errorf("expected no decisions, got %v", ds)
	}
}

func TestTargetResourceIsBuiltFromTheSource(t *testing.T) {
	got, err := Source{Kind: KindDrive, ExternalID: "0ACuIgKcIt7SPUk9PVA"}.TargetResource()
	if err != nil {
		t.Fatal(err)
	}
	if got != "//drive.googleapis.com/drives/0ACuIgKcIt7SPUk9PVA" {
		t.Errorf("target = %q", got)
	}
	if _, err := (Source{Kind: KindDrive}).TargetResource(); err == nil {
		t.Error("a source with no external id produced a target")
	}
	if _, err := (Source{Kind: "carrier-pigeon", ExternalID: "x"}).TargetResource(); err == nil {
		t.Error("an unknown kind produced a target")
	}
}

// A Meet target needs the spaces/ prefix and the stable space id. The typeable
// meeting code is an alias — tfy-qcsa-twb resolves to spaces/FS4Sj-9MIY0B — and
// targeting an alias is the same trap as matching a shared drive by name.
func TestAMeetTargetUsesTheSpaceResourceName(t *testing.T) {
	got, err := Source{Kind: KindMeet, ExternalID: "FS4Sj-9MIY0B"}.TargetResource()
	if err != nil {
		t.Fatal(err)
	}
	if got != "//meet.googleapis.com/spaces/FS4Sj-9MIY0B" {
		t.Errorf("target = %q", got)
	}
	// Idempotent when the id already carries the prefix, so a source recorded
	// either way produces one target rather than spaces/spaces/...
	got, _ = Source{Kind: KindMeet, ExternalID: "spaces/FS4Sj-9MIY0B"}.TargetResource()
	if got != "//meet.googleapis.com/spaces/FS4Sj-9MIY0B" {
		t.Errorf("prefixed id gave %q", got)
	}
}
