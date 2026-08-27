# Spike: a bearer credential that reaches the origin and resolves to a user with RLS intact

- **Date:** 2026-08-27
- **Bead:** wg-p4h.1
- **Epic:** wg-p4h — [ADR-0025](../adr/0025-api-first-external-clients.md)
- **Outcome:** origin half **proven**; edge half **blocked on a decision, and it is a bigger decision than ADR-0025 implied**
- **Code:** `internal/authn/spike_bearer.go`, `internal/authn/spike_bearer_test.go`, both behind `//go:build spike`

ADR-0025 put this bead first because two independent things could invalidate the epic and both
were cheap to test. They turned out to have very different answers, so they are reported
separately.

## Question 1 — can a non-Access credential carry an application user through the stack so that row-level security behaves exactly as it does for a browser session?

**Yes, and the reason is structural rather than lucky.**

`authz.WithUser` takes a `userID string` and nothing else:

```go
func WithUser(ctx context.Context, db *sql.DB, userID string, fn func(*sql.Tx) error) error {
	if userID == "" {
		return fmt.Errorf("refusing to open a session with no user: %w", ErrDenied)
	}
	...
	tx.ExecContext(ctx, `SELECT set_config('workgraph.user_id', $1, true)`, userID)
```

`0008_rls.sql` defines `current_app_user()` as that session setting, and every policy reads it.
So there is **no channel by which "how was this caller authenticated" reaches a policy.** If a
bearer credential and an Access JWT agree on `UserID`, every policy decides identically — not
because the policies are permissive, but because provenance is not an input they have.

That reduces the question to one the identity layer can answer, and the one gate that does branch
on provenance is four lines in `internal/domain/resolver.go`:

```go
	// A service token is a machine. It has no user record and must not acquire
	// one, or it would inherit a person's project memberships.
	if id.IsService {
		return id, nil
	}
```

This is why a service token cannot read one row of anyone's inbox: no `UserID`, so `WithUser`
refuses to open a session and the policies never get a chance to allow anything. A personal token
must take the **other** branch, which means `IsService` must be false — the credential is a person's,
not a machine's.

The spike implements exactly that and asserts it:

| Test | What it pins down |
|---|---|
| `TestBearerTokenResolvesToAPersonNotAMachine` | the identity carries a `UserID` and `IsService` is false, so the resolver's machine branch is not taken |
| `TestAccessAndBearerIdentitiesAreIndistinguishableDownstream` | an Access identity and a bearer identity for the same person are equal in `UserID` and `IsService` — the property that makes the RLS answer follow |
| `TestBearerCredentialReachesAHandlerWithAUser` | end to end through the real `authn.Middleware`, the handler sees a user |
| `TestTokenAlteredByOneByteIsRefused` | one flipped bit is a refusal, and specifically **not** a fall-through |
| `TestChainStopsAtTheFirstRecognisedRefusal` | a refused Access session cannot be rescued by a token in the same request |
| `TestEmptyChainAuthenticatesNobody` | fail-closed at both ends |
| `TestRefusalLeaksNothingAboutWhichCheckFailed` | the 401 body stays `{"error":"unauthorized"}` |

All nine spike tests pass under `-race`. The default build excludes them: `go build ./...`,
`go vet ./...` and `go test ./internal/authn/` are unchanged without `-tags spike`.

### One thing the spike found that the plan did not anticipate

**"Not recognised" and "recognised and refused" have to be different errors,** and this is not
tidiness. If a chain treats them alike, a mistyped bearer token falls through to the Access link
and the caller gets *its* refusal — so the operator is told their Access JWT is missing when their
token has a typo, and debugs the wrong credential. `errNotMine` exists for that, and
`TestTokenAlteredByOneByteIsRefused` asserts a wrong token does not fall through.

wg-p4h.3 should carry this into the real chain rather than rediscovering it.

### A second thing the plan did not anticipate: the prefix works, and it constrains the tests

The first CI run of this branch **failed the secret scan**, on this spike's own test fixture:

```
RULE   generic-api-key
FILE   internal/authn/spike_bearer_test.go
MATCH  spikeToken = "wgp_spike_..."
```

That is worth reporting rather than suppressing, because it is unplanned evidence for a decision
ADR-0025 made on reasoning alone. The ADR chose a `wgp_` prefix so that "a leaked token is
greppable in a log scan and recognisable in a paste". gitleaks found one on the first attempt, in a
test file, with no rule written for it and nobody looking. The prefix does what it was chosen to do.

It also produces a constraint that belongs to wg-p4h.2, and it is easy to get wrong in exactly the
place it happened here: **no token-shaped literal may enter the repository, test fixtures
included.** A fixture has to be composed from parts at run time, which is what
`spikeToken()` now does. Two consequences follow for the real implementation:

- the mint endpoint's tests cannot hard-code an expected token, so the assertion has to be about
  the shape and the digest rather than the value;
- `scripts/secret_scan.sh` runs `gitleaks git .` as well as `gitleaks dir .`, so a literal that
  reaches a pushed commit keeps failing CI after it is deleted from the working tree. The
  history is scanned with `fetch-depth: 0`. Removing it means rewriting the branch, which is
  cheap on an unmerged branch and expensive after a merge.

Neither is hypothetical: both were paid for on this branch.

### What question 1 did NOT prove

No PostgreSQL was involved. The claim is that provenance cannot reach a policy, which is settled by
reading `WithUser` and `current_app_user()` — not that the policies are correct, which existing
tests in `test/integration/` already cover. The stub resolver reproduces the four-line
`IsService` branch quoted above rather than using the real `Store`, which needs a database.

**Left for wg-p4h.2:** one integration test that runs a real query as `workgraph_app` with a
session identity obtained from a token, alongside the same query with one obtained from Access,
asserting identical rows. It should be a real SQL test under `test/integration/` so
`scripts/check_rls_tests.py` sees it drop privileges.

## Question 2 — can a request presenting only a bearer header reach the origin?

**Not yet, and neither of the two shapes already in the repository fits.** This is where the spike
changes the plan.

ADR-0025 said the repository "has exactly two escapes" and that the spike would pick one. Reading
them against a whole-API credential, both are wrong:

**The cell shape — one narrow Access application per path.** Used for `cell_token_mint`,
`cell_agent_health`, `cell_budget_check`, `cell_work_queue`. The module says why it is per-path,
and the comment is the finding:

> Its own application rather than a second path on the one above: a Zero Trust application matches
> one domain, and widening the existing one to a prefix would quietly extend the token's reach to
> every path underneath it.

A personal token has to work across `/v1/inbox`, `/v1/ask`, `/v1/work`, `/v1/projects` and whatever
wg-p4h.4 adds. Per-path applications mean one Access application per endpoint, forever, each a Tofu
change to ship an endpoint — and the alternative the comment warns against is exactly what a
whole-API credential needs.

**The bypass shape — `decision = "bypass"` with `everyone = {}`.** Used for the GitHub webhook,
on one exact path, because GitHub cannot complete an Access challenge. Applied to `/v1/` it would
remove Access from the entire API and make the personal token the sole boundary for every endpoint
including the ones a browser uses.

So this is a genuinely new trust-boundary decision, not a choice between two precedents.

### Recommendation, for a human to accept or reject

**A separate hostname for the token surface** — `api.<zone>` — with its own Access application
carrying the bypass policy, the interface's hostname keeping full Access unchanged, and Cloudflare
rate limiting on the new one.

Why this rather than bypassing `/v1/` on the existing host:

- the blast radius is one origin name that only ever serves token-authenticated traffic, so a
  mistake cannot silently expose the interface's session-authenticated paths;
- it is one set of Tofu resources instead of one per endpoint, so shipping an endpoint stays a code
  change;
- edge rate limiting attaches to a hostname, and wg-p4h.9 needs somewhere to put it that is not the
  origin;
- ADR-0006's "origin closed to inbound Internet traffic" stance gets one **named** exception with a
  boundary, which is a sentence that can be written down, rather than a general hole.

What it costs, stated plainly: on that hostname the token is the whole security boundary. There is
no MFA and no Access session behind it. That is the price of a credential a tool can hold, and it is
why wg-p4h.2's expiry, revocation-on-next-request and hashing are not optional, and why wg-p4h.9 is
sequenced before any external client can dispatch.

**Not done unilaterally.** Creating an Access bypass on a staging or production zone is a change to
the security boundary of a system holding client-derived material. It needs a person to agree to the
shape first, and the `wg-p4h.1` acceptance criterion — the `curl` from a machine with no cookie —
cannot be met until it exists.

## Status of the bead

`wg-p4h.1` stays open. Question 1 is answered and the code proving it is committed behind a build
tag. Question 2 needs the decision above, then a Tofu change, then the `curl`.

Nothing found here invalidates ADR-0025. The correction it does force is that the edge is a
**new** decision rather than a pick between two existing shapes, and ADR-0025's Consequences section
should say so.
