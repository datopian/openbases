package main

import (
	"strings"
	"testing"
)

// Every field is required, and the error has to say which one is missing. This
// runs once, by hand, on a host somebody has just built: "missing -admin-email"
// is the difference between fixing it in five seconds and reading the source.
func TestValidateNamesWhatIsMissing(t *testing.T) {
	full := options{orgSlug: "acme", orgName: "Acme Ltd",
		adminEmail: "ops@acme.example", adminName: "Dana Ops"}

	if err := validate(full); err != nil {
		t.Fatalf("a complete invocation was refused: %v", err)
	}

	for _, tc := range []struct {
		name string
		opt  options
		want string
	}{
		{"no slug", options{orgName: "A", adminEmail: "a@b.example", adminName: "N"}, "-org"},
		{"no org name", options{orgSlug: "a", adminEmail: "a@b.example", adminName: "N"}, "-org-name"},
		{"no admin email", options{orgSlug: "a", orgName: "A", adminName: "N"}, "-admin-email"},
		{"no admin name", options{orgSlug: "a", orgName: "A", adminEmail: "a@b.example"}, "-admin-name"},
	} {
		err := validate(tc.opt)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name %s", tc.name, err, tc.want)
		}
	}
}

// Whitespace is not a value. A slug of " " passed the "is it empty" check in an
// earlier draft and would have created an organisation nobody could refer to.
func TestValidateRejectsWhitespaceOnly(t *testing.T) {
	o := options{orgSlug: "   ", orgName: " ", adminEmail: "a@b.example", adminName: "\t"}
	err := validate(o)
	if err == nil {
		t.Fatal("whitespace-only values were accepted")
	}
	for _, want := range []string{"-org", "-org-name", "-admin-name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// An address that is not an address means the administrator can never sign in,
// and the deployment then has nobody who can fix it. Worth failing early and
// saying why, rather than creating the row and leaving a dead deployment.
func TestValidateRejectsSomethingThatIsNotAnAddress(t *testing.T) {
	o := options{orgSlug: "acme", orgName: "Acme", adminEmail: "dana", adminName: "Dana"}
	err := validate(o)
	if err == nil {
		t.Fatal("a bare username was accepted as an address")
	}
	if !strings.Contains(err.Error(), "identity provider") {
		t.Errorf("the error does not explain why the address matters: %v", err)
	}
}

func TestValidateRejectsASlugThatIsNotASlug(t *testing.T) {
	for _, bad := range []string{"acme corp", "acme/corp", "acme\tcorp"} {
		o := options{orgSlug: bad, orgName: "Acme", adminEmail: "a@b.example", adminName: "Dana"}
		if err := validate(o); err == nil {
			t.Errorf("%q was accepted as a slug", bad)
		}
	}
}

// The summary must carry the one thing an installer cannot guess: no login
// identity exists, none can be created in advance, and the address has to match
// what the provider asserts or the administrator is locked out. A tool that
// creates an admin and says only "done" produces exactly that dead deployment.
func TestSummaryExplainsHowTheFirstSignInWorks(t *testing.T) {
	o := options{orgSlug: "acme", orgName: "Acme Ltd",
		adminEmail: "ops@acme.example", adminName: "Dana Ops"}

	got := summary("created", o)
	for _, want := range []string{"ops@acme.example", "organisation_admin", "first sign-in", "refused"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary does not mention %q:\n%s", want, got)
		}
	}

	// Re-running is expected and must read as a no-op, not as success at
	// having created something. An installer who sees "created" twice has to
	// go and check whether they made two organisations.
	again := summary("already_initialised", o)
	if !strings.Contains(again, "Nothing to do") {
		t.Errorf("a repeat run does not read as a no-op:\n%s", again)
	}
	if strings.Contains(again, "created organisation") {
		t.Errorf("a repeat run claims to have created something:\n%s", again)
	}

	// Portfolios are opt-in, so the summary must not claim them when they were
	// not asked for.
	if strings.Contains(summary("created", o), "portfolios:") {
		t.Error("portfolios reported without -with-portfolios")
	}
	o.portfolios = true
	if !strings.Contains(summary("created", o), "portfolios:") {
		t.Error("portfolios not reported when asked for")
	}
}
