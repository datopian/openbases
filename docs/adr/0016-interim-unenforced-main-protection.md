# ADR-0016: Interim — `main` protection is unenforceable, with compensating controls

- **Status:** accepted
- **Date:** 2026-08-10
- **Bead:** wg-8yv.30
- **Plan reference:** §16.3, §18 WP-A1
- **Relates to:** [ADR-0015](0015-gitops-immutable-production.md)

## Context

ADR-0015 and plan §16.3 require that `main` is protected and that direct pushes fail. GitHub does
not permit this: branch protection rules and repository rulesets on **private** repositories require
GitHub Team or Enterprise, and the `datopian` organisation is on the Free plan. Both API calls
return:

```
403 Upgrade to GitHub Pro or make this repository public to enable this feature.
```

Environment required-reviewer protection rules are blocked by the same limit, so the `production`
environment cannot require an approver either.

The three available responses were: upgrade the organisation to GitHub Team; keep the repositories
private without enforcement; or make them public. Making them public is rejected outright —
`company-workgraph` will hold client-derived knowledge. Datopian leadership chose to proceed without
enforcement for now.

## Decision

Proceed with `main` unprotected, and treat the gap as an **explicit, recorded accepted risk** rather
than an oversight. The WP-A1 acceptance criterion "direct pushes to `main` fail" remains **unmet**,
and blocker Bead `wg-8yv.30` stays open until the plan is upgraded.

Three compensating controls apply in the meantime:

1. **Detection, not prevention.** A workflow runs on every push to `main` and fails loudly when a
   pushed commit is not associated with a merged pull request. It cannot stop the push, but the
   push cannot happen unnoticed.
2. **Local prevention.** `make bootstrap` installs a `pre-push` hook that refuses a push to `main`.
   This stops the accidental case, which is the common one. It does not stop a determined actor,
   and it is not a security control.
3. **Honest evidence.** `docs/go-live/GO_LIVE_EVIDENCE.md` records this as a known limitation. It
   is not marked complete.

## Consequences

- Approval integrity now rests on convention plus detection. An actor with write access can bypass
  review; they will be visible in the audit trail, not blocked at the gate.
- The self-improvement guardrail that "nothing approves itself" is weakened in exactly the way
  ADR-0014 warns about: a self-improvement pull request could, in principle, be merged without the
  required human. The policy still forbids it and the detection workflow will surface it, but the
  mechanical block is absent.
- This must be revisited before the pilot handles a restricted client project. A protected `main` is
  a precondition for the production approval path, not a nicety.
- If the plan is upgraded, apply protection and close `wg-8yv.30`; this ADR is then superseded.

## Alternatives considered

**Upgrade to GitHub Team.** The correct fix, deferred on cost. The organisation reports 219 filled
seats, so the decision needs a check of actually billable members.

**Move the two repositories to a separately paid org or account.** Rejected for now: it splits them
from the `datopian` organisation's identity, SSO, and team access for a control that the plan
upgrade would provide anyway.

**Make the repositories public.** Rejected. Plan §18 WP-A1 requires them private, and
`company-workgraph` is designed to hold client-derived knowledge.
