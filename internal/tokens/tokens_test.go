package tokens

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/datopian/workgraph/internal/authz"
)

// A fixture is composed at run time, never written as a literal.
//
// wg-p4h.1's first CI run failed the secret scan on a wgp_-prefixed constant in
// a test file, flagged generic-api-key with no rule written for it. That is
// evidence the prefix works, and it makes a literal here a CI failure that
// survives being deleted from the working tree, because secret_scan.sh scans
// history as well. So tests build one.
func fixtureSecret(seed string) string {
	raw := sha256.Sum256([]byte("wg-p4h.2 fixture/" + seed))
	return Prefix + base64.RawURLEncoding.EncodeToString(raw[:])
}

func TestGeneratedTokenIsPrefixedAndFullWidth(t *testing.T) {
	secret, digest, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, Prefix) {
		t.Fatalf("secret %q lacks the %q prefix that makes a leak greppable", secret, Prefix)
	}
	if got := sha256.Sum256([]byte(secret)); got != digest {
		t.Fatal("the digest is not the digest of the secret")
	}
	if _, ok := digestOf(secret); !ok {
		t.Fatal("a freshly generated secret is not recognised by digestOf")
	}
}

func TestTwoTokensNeverCollide(t *testing.T) {
	seen := make(map[string]struct{}, 512)
	for i := 0; i < 512; i++ {
		s, _, err := generate()
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seen[s]; dup {
			t.Fatal("generate produced a duplicate secret")
		}
		seen[s] = struct{}{}
	}
}

// Shape is checked before the database is touched, so an unrelated bearer token
// costs a string comparison rather than a round trip.
func TestOnlyOurShapeReachesTheDatabase(t *testing.T) {
	valid := fixtureSecret("shape")
	for _, tc := range []struct {
		name, secret string
		want         bool
	}{
		{"ours", valid, true},
		{"no prefix", strings.TrimPrefix(valid, Prefix), false},
		// Composed, not written. A literal here is flagged github-pat and,
		// once committed, keeps failing CI after it is deleted from the working
		// tree, because secret_scan.sh scans history too (wg-cjp).
		{"someone else's bearer", "ghp" + "_" + strings.Repeat("0123456789", 4), false},
		{"empty", "", false},
		{"prefix only", Prefix, false},
		{"truncated", valid[:len(valid)-1], false},
		{"overlong", valid + "x", false},
	} {
		if _, ok := digestOf(tc.secret); ok != tc.want {
			t.Errorf("%s: digestOf recognised=%v, want %v", tc.name, ok, tc.want)
		}
	}
}

// One flipped character must not resolve to the same digest.
func TestOneAlteredCharacterChangesTheDigest(t *testing.T) {
	secret := fixtureSecret("altered")
	a, ok := digestOf(secret)
	if !ok {
		t.Fatal("fixture is not the right shape")
	}
	altered := []byte(secret)
	altered[len(altered)-1] ^= 1
	b, ok := digestOf(string(altered))
	if !ok {
		t.Fatal("altered fixture changed shape, so this asserts nothing")
	}
	if a == b {
		t.Fatal("a one-character change produced the same digest")
	}
}

// The rule the approval model rests on. ADR-0009 binds an approval to a digest
// of the action; an agent holding its owner's token approving its own dispatch
// would defeat that, and no scope value makes it safe.
func TestApprovalDecideIsNeverGrantable(t *testing.T) {
	err := ValidateScopes([]string{string(authz.ApprovalDecide)})
	if !errors.Is(err, ErrProtectedScope) {
		t.Fatalf("approval.decide was accepted as a token scope: %v", err)
	}
}

func TestEveryProtectedActionIsRefused(t *testing.T) {
	protected := []authz.Action{
		authz.ApprovalDecide,
		authz.PullRequestMerge,
		authz.DeploymentExecute,
		authz.SecretManage,
		authz.PolicyManage,
		authz.MarketingPublish,
		authz.KnowledgeClassificationDowngrade,
	}
	for _, a := range protected {
		if err := ValidateScopes([]string{string(a)}); !errors.Is(err, ErrProtectedScope) {
			t.Errorf("%s was accepted as a token scope: %v", a, err)
		}
	}
}

// approval.decide is NOT Protected() in internal/authz, and that is correct:
// deciding an approval cannot itself require an approval without becoming
// circular. It must still be ungrantable to a token, for a different reason.
//
// This test exists because the first version of ValidateScopes consulted only
// Protected() and therefore accepted approval.decide — the single most
// dangerous scope in the set. Pinning the distinction so it cannot regress.
func TestApprovalDecideIsUngrantableEvenThoughItIsNotProtected(t *testing.T) {
	if authz.ApprovalDecide.Protected() {
		t.Fatal("approval.decide became Protected(); this test's premise needs revisiting, " +
			"but the scope must stay ungrantable either way")
	}
	if err := ValidateScopes([]string{string(authz.ApprovalDecide)}); !errors.Is(err, ErrProtectedScope) {
		t.Fatal("approval.decide is not Protected() and was therefore accepted as a scope, " +
			"which lets an agent approve its own dispatch and defeats ADR-0009")
	}
}

// A future action marked Protected() in internal/authz must be refused here
// without anyone remembering to add it to Ungrantable.
func TestRefusalTracksAuthzRatherThanACopiedList(t *testing.T) {
	for _, name := range []string{
		"organisation.read", "project.read", "project.manage", "work.create",
		"work.update", "work.assign", "agent.dispatch", "agent.inspect",
		"agent.stop", "repository.read", "pull_request.create",
		"approval.decide", "deployment.execute", "secret.manage",
		"policy.manage", "audit.read", "marketing.publish",
		"pull_request.merge", "knowledge.classification.downgrade",
	} {
		a := authz.Action(name)
		_, ungrantable := Ungrantable[a]
		err := ValidateScopes([]string{name})
		if a.Protected() || ungrantable {
			if !errors.Is(err, ErrProtectedScope) {
				t.Errorf("%s must not be grantable to a token, but was accepted", name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s is ordinary but was refused: %v", name, err)
		}
	}
}

// The Go set and the SQL CHECK must agree, or a scope refused in one place is
// accepted in the other.
func TestUngrantableMatchesTheMigration(t *testing.T) {
	// Whichever migration defines the constraint LAST is the authority, not
	// 0038. Migrations are immutable, so adding a scope means redefining the
	// CHECK in a new file -- and a test pinned to 0038 reports that correct
	// change as a mismatch, which is what it did when knowledge.review was
	// added in 0054.
	files, err := filepath.Glob("../../db/migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Skipf("migrations not readable from here: %v", err)
	}
	sort.Strings(files)

	var authority, body string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		// Matched on the CHECK's shape rather than its name: the constraint
		// was renamed once already, and a guard pinned to a name reports the
		// rename as a missing constraint.
		if strings.Contains(string(b), "CHECK (NOT (scopes") {
			authority, body = filepath.Base(f), string(b)
		}
	}
	if authority == "" {
		t.Fatal("no migration defines api_tokens_scopes_check; the Go set has no counterpart")
	}

	for a := range Ungrantable {
		if !strings.Contains(body, "'"+string(a)+"'") {
			t.Errorf("%s is ungrantable in Go but absent from the CHECK in %s", a, authority)
		}
	}

	// And the other direction, which the old version never checked: a scope
	// refused by the database but not by Go surfaces as a constraint violation
	// instead of a named refusal.
	for _, m := range regexp.MustCompile(`'([a-z_]+\.[a-z_.]+)'`).FindAllStringSubmatch(
		body[strings.Index(body, "CHECK (NOT (scopes"):], -1) {
		if _, ok := Ungrantable[authz.Action(m[1])]; !ok {
			t.Errorf("%s is refused by %s but grantable in Go", m[1], authority)
		}
	}
}

func TestUnknownActionIsRefused(t *testing.T) {
	if err := ValidateScopes([]string{"work.destroy"}); err == nil {
		t.Fatal("an unrecognised action was accepted; the set is meant to be closed")
	}
}

// The original test above asserted only that an error came back, which is how
// an unknown scope reached the API as a 500 "internal error" for a fortnight:
// the handler had nothing to match on, so it fell through to the generic
// branch. The sentinel is the contract, so the sentinel is what is asserted.
func TestUnknownActionIsRefusedWithASentinelAndTheOffendingName(t *testing.T) {
	err := ValidateScopes([]string{"work.destroy"})
	if !errors.Is(err, ErrUnknownScope) {
		t.Fatalf("want ErrUnknownScope so a handler can answer 400, got %v", err)
	}
	if !strings.Contains(err.Error(), "work.destroy") {
		t.Errorf("the refusal should name what was refused, got %q", err)
	}
	if errors.Is(err, ErrProtectedScope) {
		t.Error("a typo is not a protected action; conflating them gives the caller the wrong advice")
	}
}

// These three are the names an agent actually guessed against the live API.
// They read like they should exist, which is why the refusal has to be legible
// rather than a 500.
func TestPlausibleButWrongScopeNamesAreUnknownNotProtected(t *testing.T) {
	for _, s := range []string{"project.write", "work.read", "work.write"} {
		if !errors.Is(ValidateScopes([]string{s}), ErrUnknownScope) {
			t.Errorf("%s: expected an unknown-scope refusal", s)
		}
	}
}

func TestGrantableScopesExcludesEveryUngrantableAction(t *testing.T) {
	grantable := GrantableScopes()
	if len(grantable) == 0 {
		t.Fatal("no grantable scope at all would make every token useless")
	}
	for _, s := range grantable {
		if err := ValidateScopes([]string{s}); err != nil {
			t.Errorf("%s is advertised as grantable but ValidateScopes refuses it: %v", s, err)
		}
	}
	// The hint must not name something the caller would then be refused for.
	for a := range Ungrantable {
		if slices.Contains(grantable, string(a)) {
			t.Errorf("%s is ungrantable but advertised as grantable", a)
		}
	}
	for _, a := range authz.AllActions() {
		if a.Protected() && slices.Contains(grantable, string(a)) {
			t.Errorf("%s is protected but advertised as grantable", a)
		}
	}
}

func TestGrantableScopesIsSortedSoTheHintIsStable(t *testing.T) {
	got := GrantableScopes()
	if !slices.IsSorted(got) {
		t.Errorf("the hint is read by people and diffed in tests; want sorted, got %v", got)
	}
}

func TestEmptyScopesAreValid(t *testing.T) {
	if err := ValidateScopes(nil); err != nil {
		t.Fatalf("a read-only token should be valid: %v", err)
	}
	if err := ValidateScopes([]string{}); err != nil {
		t.Fatalf("a read-only token should be valid: %v", err)
	}
}

func TestLiveReportsRevocationAndExpiry(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	revoked := now.Add(-time.Hour)
	for _, tc := range []struct {
		name string
		tok  Token
		want bool
	}{
		{"valid", Token{ExpiresAt: now.Add(time.Hour)}, true},
		{"expired", Token{ExpiresAt: now.Add(-time.Second)}, false},
		{"revoked", Token{ExpiresAt: now.Add(time.Hour), RevokedAt: &revoked}, false},
		{"revoked and expired", Token{ExpiresAt: now.Add(-time.Hour), RevokedAt: &revoked}, false},
	} {
		if got := tc.tok.Live(now); got != tc.want {
			t.Errorf("%s: Live=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTextArrayRoundTrips(t *testing.T) {
	for _, in := range [][]string{
		{},
		{"work.create"},
		{"work.create", "agent.dispatch", "project.read"},
	} {
		if got := parseTextArray(pgTextArray(in)); len(got) != len(in) {
			t.Fatalf("round trip of %v produced %v", in, got)
		}
	}
	if got := parseTextArray("{}"); len(got) != 0 {
		t.Fatalf("empty array parsed as %v", got)
	}
	if got := parseTextArray(""); len(got) != 0 {
		t.Fatalf("empty string parsed as %v", got)
	}
}

// MaxLifetime is duplicated as a CHECK constraint in 0038_api_tokens.sql. If
// one moves, the other has to, and this is the reminder.
func TestMaxLifetimeMatchesTheConstraint(t *testing.T) {
	if MaxLifetime != 90*24*time.Hour {
		t.Fatalf("MaxLifetime is %s; 0038_api_tokens.sql enforces 90 days", MaxLifetime)
	}
}
