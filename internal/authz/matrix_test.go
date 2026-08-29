package authz

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The matrix exists in two places: the table in ADR-0026, which a human read and
// accepted, and the INSERT in 0040_role_permissions.sql, which the system
// enforces. If they disagree, the enforced permissions are not the reviewed ones
// and the review was worthless.
//
// The migration was generated from the ADR rather than retyped, so they agree
// today. This is what keeps them agreeing after somebody edits one of them.
func TestMigrationMatchesTheAcceptedMatrix(t *testing.T) {
	adr := readFile(t, "../../docs/adr/0026-role-permission-matrix.md")
	mig := readFile(t, "../../db/migrations/0040_role_permissions.sql")

	fromADR := grantsFromADR(t, adr)
	fromSQL := grantsFromMigration(t, mig)

	if len(fromADR) == 0 {
		t.Fatal("parsed no grants from ADR-0026; the table format has changed and this test is asserting nothing")
	}

	missing := difference(fromADR, fromSQL)
	extra := difference(fromSQL, fromADR)

	if len(missing) > 0 {
		t.Errorf("granted by ADR-0026 but absent from the migration:\n  %s",
			strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("granted by the migration but NOT by ADR-0026:\n  %s\n\n"+
			"These are permissions nobody reviewed.", strings.Join(extra, "\n  "))
	}
}

// Every action in the matrix must be one internal/authz recognises. The
// migration deliberately has no foreign key to an actions table — the
// vocabulary lives in Go as a closed set — so this is what stops a typo
// becoming a permission that silently never matches.
func TestEveryGrantedActionIsKnown(t *testing.T) {
	for _, g := range grantsFromMigration(t, readFile(t, "../../db/migrations/0040_role_permissions.sql")) {
		action := g[strings.Index(g, "/")+1:]
		if !Action(action).Known() {
			t.Errorf("%s grants %q, which internal/authz does not recognise", g, action)
		}
	}
}

// The decisions ADR-0026 called out explicitly. If one of these flips, it should
// be because somebody wrote another ADR, not because a line moved.
func TestTheRulingsThatWereFlaggedForReview(t *testing.T) {
	granted := map[string]bool{}
	for _, g := range grantsFromADR(t, readFile(t, "../../docs/adr/0026-role-permission-matrix.md")) {
		granted[g] = true
	}

	for _, tc := range []struct {
		grant string
		want  bool
		why   string
	}{
		{"organisation_admin/approval.decide", false,
			"an account that changes policy and holds every secret must not also approve under it (ADR-0009)"},
		{"external_client/approval.decide", true,
			"plan §8.2 says external clients are read-only OR approval-only"},
		{"organisation_admin/pull_request.merge", true, "administers all projects"},
		{"organisation_admin/marketing.publish", false,
			"publishing in the company's name is not an infrastructure administrator's call"},
		{"executive/knowledge.classification.downgrade", true,
			"plan §14.3, and given to exactly one role"},
		{"organisation_admin/knowledge.classification.downgrade", false,
			"the most dangerous non-technical action in the set is not an admin convenience"},
		{"executive/work.create", false, "§8.2 gives the executive visibility and approvals, not execution"},
		{"contributor/agent.dispatch", false, "dispatch spends money; revisit when wg-p4h.9 caps it"},
		{"portfolio_lead/audit.read", false, "audit records span projects and carry other people's actions"},
	} {
		if granted[tc.grant] != tc.want {
			t.Errorf("%s = %v, want %v — %s\n  This was a reviewed ruling in ADR-0026; changing it needs another ADR.",
				tc.grant, granted[tc.grant], tc.want, tc.why)
		}
	}
}

// Protected actions must be narrowly held. Not a rule about which roles, but a
// smell test: if a protected action is granted to most of the organisation, the
// approval it requires has stopped being a meaningful gate.
func TestProtectedActionsAreNarrowlyHeld(t *testing.T) {
	counts := map[string]int{}
	for _, g := range grantsFromADR(t, readFile(t, "../../docs/adr/0026-role-permission-matrix.md")) {
		action := g[strings.Index(g, "/")+1:]
		counts[action]++
	}
	const roles = 9
	for action, n := range counts {
		if !Action(action).Protected() {
			continue
		}
		if n > roles/2 {
			t.Errorf("%s is protected but granted to %d of %d roles; an approval most people can give "+
				"is not much of a gate", action, n, roles)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

var roleOrder = []string{
	"organisation_admin", "executive", "portfolio_lead", "function_lead",
	"project_lead", "backup_operator", "contributor", "observer", "external_client",
}

// grantsFromADR reads the markdown table. A cell is a grant when it is • or !.
func grantsFromADR(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != len(roleOrder)+1 {
			t.Fatalf("ADR row has %d cells, expected %d: %s", len(cells), len(roleOrder)+1, line)
		}
		action := strings.Trim(strings.TrimSpace(cells[0]), "`")
		for i, role := range roleOrder {
			switch strings.TrimSpace(cells[i+1]) {
			case "•", "!":
				out = append(out, role+"/"+action)
			}
		}
	}
	sort.Strings(out)
	return out
}

var insertRow = regexp.MustCompile(`\('([a-z_]+)',\s*'([a-z_.]+)'\)`)

func grantsFromMigration(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, m := range insertRow.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1]+"/"+m[2])
	}
	sort.Strings(out)
	return out
}

func difference(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}
