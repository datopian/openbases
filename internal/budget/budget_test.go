package budget

import (
	"strings"
	"testing"
	"time"
)

func fresh(kind, key, limit, spent string, exceeded bool) Status {
	return Status{
		SubjectKind: kind, SubjectKey: key,
		DailyCents: limit, SpentCents: spent,
		RemainingCents: "0", Exceeded: exceeded,
		StalenessSeconds: 60,
	}
}

func TestAnExceededBudgetRefuses(t *testing.T) {
	s := fresh("bead", "wg-qw1", "200.0000", "214.5500", true)
	d := Decide(s, DefaultPolicy())
	if d.Allow {
		t.Fatal("dispatch was allowed against a spent budget")
	}
	for _, want := range []string{"bead", "wg-qw1", "214.55", "200"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("the refusal should say %q: %s", want, d.Reason)
		}
	}
}

// Over the line is over the line whatever the data's age: more spend has
// happened since the import, never less. A stale "exceeded" that got downgraded
// to a warning would authorise work that is already over.
func TestAnExceededBudgetRefusesEvenOnStaleData(t *testing.T) {
	s := fresh("project", "portaljs-oss", "500", "600", true)
	s.StalenessSeconds = int64((30 * time.Hour).Seconds())
	if Decide(s, DefaultPolicy()).Allow {
		t.Fatal("a day-old 'exceeded' was treated as unknown rather than as exceeded")
	}
}

// The failure this package exists to prevent: authorising work confidently on
// spend data old enough to be wrong.
func TestStaleDataRefusesRatherThanGuessing(t *testing.T) {
	s := fresh("project", "portaljs-oss", "500", "10", false)
	s.RemainingCents = "490"
	s.StalenessSeconds = int64((3 * time.Hour).Seconds())

	d := Decide(s, DefaultPolicy())
	if d.Allow {
		t.Fatalf("allowed on data 3h old with a 2h trust window: %s", d.Reason)
	}
	if !strings.Contains(d.Reason, "3h0m0s") || !strings.Contains(d.Reason, "2h0m0s") {
		t.Errorf("the refusal should name both ages: %s", d.Reason)
	}
}

func TestNothingEverImportedIsNotFreshness(t *testing.T) {
	s := fresh("project", "p", "500", "0", false)
	s.RemainingCents = "500"
	s.StalenessSeconds = -1
	d := Decide(s, DefaultPolicy())
	if d.Allow {
		t.Fatal("an empty usage table was treated as 'nothing spent'")
	}
	if !strings.Contains(d.Reason, "ever been imported") {
		t.Errorf("the reason should say why: %s", d.Reason)
	}
}

// Unbudgeted work is allowed by default and always reported. The alternative —
// refuse everything until every project has a budget — is how enforcement gets
// switched off wholesale on its first day.
func TestUnbudgetedWorkIsAllowedButNeverSilent(t *testing.T) {
	s := Status{SubjectKind: "none", SpentCents: "0", StalenessSeconds: 60}
	d := Decide(s, DefaultPolicy())
	if !d.Allow {
		t.Fatal("unbudgeted work was refused under the default policy")
	}
	if d.Warning == "" {
		t.Fatal("unbudgeted work was allowed with no warning, which makes it invisible")
	}

	strict := DefaultPolicy()
	strict.AllowUnbudgeted = false
	if Decide(s, strict).Allow {
		t.Fatal("unbudgeted work was allowed under a policy that forbids it")
	}
}

// Staleness must not block work that has no budget to check in the first place:
// refusing there would mean a broken importer stops all dispatch everywhere,
// which is a much bigger outage than the one it prevents.
func TestStalenessDoesNotBlockUnbudgetedWork(t *testing.T) {
	s := Status{SubjectKind: "none", SpentCents: "0", StalenessSeconds: int64((99 * time.Hour).Seconds())}
	if !Decide(s, DefaultPolicy()).Allow {
		t.Fatal("stale data blocked work that had no budget to check")
	}
}

func TestApproachingTheCeilingWarnsWithoutRefusing(t *testing.T) {
	s := fresh("bead", "wg-qw1", "200", "170", false) // 85%
	s.RemainingCents = "30"
	d := Decide(s, DefaultPolicy())
	if !d.Allow {
		t.Fatalf("85%% of a budget was treated as spent: %s", d.Reason)
	}
	if !strings.Contains(d.Warning, "85%") {
		t.Errorf("expected an 85%% warning, got %q", d.Warning)
	}

	quiet := fresh("bead", "wg-qw1", "200", "10", false)
	quiet.RemainingCents = "190"
	if w := Decide(quiet, DefaultPolicy()).Warning; w != "" {
		t.Errorf("5%% of a budget produced a warning: %q", w)
	}
}

// A zero ceiling means stop, and it must not divide by zero on the way there.
func TestAZeroBudgetIsAStop(t *testing.T) {
	s := fresh("bead", "wg-frozen", "0", "0", true)
	d := Decide(s, DefaultPolicy())
	if d.Allow {
		t.Fatal("a zero budget allowed dispatch")
	}
}

// Money is rendered for a person who is being told to stop, not for a machine.
func TestAmountsAreReadable(t *testing.T) {
	for in, want := range map[string]string{
		"200.0000": "200", "214.5500": "214.55", "0.0088": "0.0088",
		"0.0000": "0", "": "", "not a number": "not a number",
	} {
		if got := trim(in); got != want {
			t.Errorf("trim(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStalenessCheckCanBeDisabled(t *testing.T) {
	s := fresh("project", "p", "500", "10", false)
	s.RemainingCents = "490"
	s.StalenessSeconds = int64((99 * time.Hour).Seconds())

	p := DefaultPolicy()
	p.MaxStaleness = 0
	if !Decide(s, p).Allow {
		t.Fatal("staleness blocked dispatch under a policy that does not check it")
	}
}

func TestPolicyFromEnv(t *testing.T) {
	t.Setenv("WG_BUDGET_MAX_STALENESS", "30m")
	t.Setenv("WG_BUDGET_ALLOW_UNBUDGETED", "false")
	t.Setenv("WG_BUDGET_WARN_PERCENT", "50")

	p := PolicyFromEnv()
	if p.MaxStaleness != 30*time.Minute {
		t.Errorf("MaxStaleness = %s, want 30m", p.MaxStaleness)
	}
	if p.AllowUnbudgeted {
		t.Error("AllowUnbudgeted stayed true against WG_BUDGET_ALLOW_UNBUDGETED=false")
	}
	if p.WarnAtPercent != 50 {
		t.Errorf("WarnAtPercent = %v, want 50", p.WarnAtPercent)
	}
}

// A nonsense value must leave the default in place rather than silently
// becoming zero — "0" means "do not check staleness at all", and reaching that
// by typo would switch off the check while looking configured.
func TestAnUnparseableThresholdKeepsTheDefault(t *testing.T) {
	t.Setenv("WG_BUDGET_MAX_STALENESS", "two hours")
	if got := PolicyFromEnv().MaxStaleness; got != DefaultPolicy().MaxStaleness {
		t.Errorf("MaxStaleness = %s after a bad value, want the default %s",
			got, DefaultPolicy().MaxStaleness)
	}
}

func TestZeroStalenessIsRespectedAsADeliberateChoice(t *testing.T) {
	t.Setenv("WG_BUDGET_MAX_STALENESS", "0")
	if got := PolicyFromEnv().MaxStaleness; got != 0 {
		t.Errorf("MaxStaleness = %s, want 0 — an explicit 0 disables the check", got)
	}
}

// The operational limits must reach the caller, because the caller is the only
// one who can enforce them: concurrency is a property of the node, not of the
// control plane. Stored since 0006 and read by nothing until wg-726.
func TestTheDecisionCarriesTheOperationalLimits(t *testing.T) {
	s := fresh("project", "portaljs-oss", "5000", "10", false)
	s.RemainingCents = "4990"
	s.MaxAgents = 2
	s.MaxRuntimeMinutes = 45

	d := Decide(s, DefaultPolicy())
	if !d.Allow {
		t.Fatalf("refused: %s", d.Reason)
	}
	if d.MaxAgents != 2 || d.MaxRuntimeMinutes != 45 {
		t.Fatalf("the limits did not reach the decision: agents=%d runtime=%d",
			d.MaxAgents, d.MaxRuntimeMinutes)
	}
}

// A budget that sets no limits must report none rather than zero-as-a-ceiling,
// which the caller would read as "no agents may run".
func TestABudgetWithNoLimitsCarriesNone(t *testing.T) {
	s := fresh("cell", "oss", "10000", "0", false)
	s.RemainingCents = "10000"
	d := Decide(s, DefaultPolicy())
	if d.MaxAgents != 0 || d.MaxRuntimeMinutes != 0 {
		t.Fatalf("limits were invented: agents=%d runtime=%d", d.MaxAgents, d.MaxRuntimeMinutes)
	}
}
