# ADR-0026: The role-to-action matrix

- **Status:** proposed — **this is a draft for a human to correct, not an agent's decision**
- **Date:** 2026-08-29
- **Bead:** wg-p4h.12
- **Plan reference:** §8.2, §14.3, §10.1
- **Relates to:** [ADR-0009](0009-approval-digest-binding.md), [ADR-0014](0014-governed-self-improvement.md), [ADR-0025](0025-api-first-external-clients.md)

## Context

`internal/authz` defines nineteen actions as a closed set and marks six protected. `0001_core.sql`
seeds nine roles as `(name, description)`, and `role_grants` links a user to a role at organisation
or project scope. **Nothing anywhere connects the two.** `grep` for `agent.dispatch` outside
`internal/authz` and `internal/tokens` returns nothing.

That is the honest reason `authz` had no callers in the HTTP layer until `wg-p4h.3`: there was
nothing to call it with. `authz.Authorizer`'s own comment claims "the production implementation
reads scoped role grants from PostgreSQL", and no such implementation exists — only the in-memory
`GrantSet` used by tests.

Plan §8.2 lists the roles and lists the actions, in prose, and deliberately does not map them. This
is that mapping, proposed.

**Why an agent should not simply decide this.** Six of the nineteen actions are
`authz.Action.Protected()`, meaning they normally require a durable human approval, so the matrix
also decides *who can be asked for that approval*. ADR-0014's guarantee that nothing approves its
own protected change depends on the answer. Getting a cell wrong here is not a bug that surfaces in
a test; it is a permission somebody has and nobody noticed.

So this is written to be **corrected**, and the section after the matrix names every cell I am not
confident about, with the argument on both sides.

## Decision (proposed)

`•` granted · `—` not granted · `!` protected action, granted but still requiring an approval

Scope: a grant is evaluated at organisation or project scope per `role_grants`. Row-level security
independently bounds *which rows* a caller sees; this matrix bounds *what they may do* to the rows
they can already see. Both must pass.

| Action | org&nbsp;admin | executive | portfolio&nbsp;lead | function&nbsp;lead | project&nbsp;lead | backup&nbsp;operator | contributor | observer | external&nbsp;client |
|---|---|---|---|---|---|---|---|---|---|
| `organisation.read` | • | • | • | • | — | — | — | — | — |
| `project.read` | • | • | • | • | • | • | • | • | • |
| `project.manage` | • | — | • | • | • | — | — | — | — |
| `work.create` | • | — | • | • | • | • | • | — | — |
| `work.update` | • | — | • | • | • | • | • | — | — |
| `work.assign` | • | — | • | • | • | • | — | — | — |
| `agent.dispatch` | • | — | • | • | • | • | — | — | — |
| `agent.inspect` | • | • | • | • | • | • | • | • | — |
| `agent.stop` | • | — | • | • | • | • | — | — | — |
| `repository.read` | • | — | • | • | • | • | • | — | — |
| `pull_request.create` | • | — | • | • | • | • | • | — | — |
| `pull_request.merge` | ! | — | ! | — | ! | ! | — | — | — |
| `approval.decide` | — | • | • | • | • | • | — | — | • |
| `deployment.execute` | ! | — | — | — | ! | ! | — | — | — |
| `secret.manage` | ! | — | — | — | — | — | — | — | — |
| `policy.manage` | ! | — | — | — | — | — | — | — | — |
| `audit.read` | • | • | — | — | — | • | — | — | — |
| `marketing.publish` | — | ! | — | ! | — | — | — | — | — |
| `knowledge.classification.downgrade` | — | ! | — | — | — | — | — | — | — |

### The three principles behind it

**Separation of duties beats convenience.** The organisation admin does *not* hold
`approval.decide`. Admin can change policy and hold secrets; letting the same person also approve
actions *under* that policy collapses ADR-0009's guarantee into one account. This is the single
biggest departure from "admin can do everything", and it is deliberate.

**Read is cheap, spend is not.** `agent.inspect` is broad — anyone who can see a project can see
what its agents are doing. `agent.dispatch` is narrow, because dispatching costs money and
`wg-p4h.9` exists precisely because that cost has no ceiling yet. Contributors read and write work,
and do not start agents.

**Protected actions are grants to be *asked*, not to act unilaterally.** A `!` means the role may
be the actor once an approval exists — never that the approval is skipped. `secret.manage` and
`policy.manage` are admin-only *and* protected, so even the admin acts against a recorded decision.

## The cells I am least sure of — please rule on these

These are the ones worth your attention. The rest of the matrix follows fairly directly from §8.2's
role descriptions.

**1. `approval.decide` for `organisation_admin`: proposed NO.** *For:* an admin locked out of
approvals cannot unblock anything at 3am, and there may be only one of them. *Against:* the admin
defines policy and holds every secret; if they also approve, no protected action has an independent
check. I chose separation. If the team is small enough that this is impractical, the honest fix is a
break-glass path that is audited, not a permanent grant.

**2. `approval.decide` for `external_client`: proposed YES.** §8.2 says external clients are
"read-only **or approval-only**", so this is the plan's own words. But it means a non-employee can
satisfy a protected action. Correct for "client signs off on their own deliverable", wrong if it
ever leaks to a broader action class.

**3. `pull_request.merge` for `organisation_admin`: proposed YES (protected).** Consistent with
administering all projects. Arguable that merging is project work, not administration.

**4. `marketing.publish` for `organisation_admin`: proposed NO.** Publishing in the company's name
is a function lead's or executive's call, not an infrastructure administrator's. Easy to disagree
with.

**5. `knowledge.classification.downgrade`: proposed EXECUTIVE ONLY.** Plan §14.3 requires "an
authorised human with classification-downgrade permission" and this is the most dangerous
non-technical action in the set — it is how restricted client material becomes publishable. I gave
it to exactly one role. Possibly it should be a per-person grant rather than a role at all.

**6. `work.create` for `executive`: proposed NO.** §8.2 gives the executive "portfolio visibility,
approvals, sensitive summaries" — no execution. But an executive who cannot file a bead will ask
someone else to, and the graph loses the requester.

**7. `agent.dispatch` for `contributor`: proposed NO.** The cost argument above. Against it: a
contributor who cannot dispatch cannot actually do the work Workgraph exists to route to them.
Revisit once `wg-p4h.9`'s per-token spend caps exist, which may make this safe.

**8. `audit.read` for `portfolio_lead`: proposed NO.** Audit records span projects and carry other
people's actions. A portfolio lead sees their portfolio's work through RLS already.

## Consequences

Once agreed, this becomes `role_permissions` seeded by migration, an `authz.Authorizer` over it, and
the second half of the check in `cmd/control-api`. That is roughly a day's work and is not the hard
part.

**Nothing that works today breaks when it lands**, provided the matrix is at least as permissive as
current behaviour. Today a person's reach is bounded only by row-level security; adding the matrix
can only narrow it. Each of the nine roles needs at least one real user checked against it before
enforcement is switched on, or the first person to notice will be someone locked out mid-task.

**A token can never exceed this.** `wg-p4h.3` made a token's scopes a ceiling on the credential;
what runs is the intersection of that ceiling and the owner's grants here. The matrix is therefore
the upper bound on what any external client can ever do.

**Roles are not the whole story.** `knowledge.classification.downgrade` in particular may deserve to
be a per-person grant with an expiry rather than a role attribute — plan §14.3 speaks of a person
holding the permission, not a role that implies it. Recorded here rather than resolved.

## Alternatives considered

**Give the organisation admin everything.** Simplest, and how most systems start. Rejected because
it makes one account the sole holder of policy, secrets and approval, and ADR-0016 already records
that this repository has no mechanical block on a bypass — convention is the control, so the
permission model should not hand one account the ability to end-run it.

**Per-user grants with no roles.** Maximum precision, and `role_grants` would still carry them.
Rejected as an operational burden at nine people: nobody would keep it correct, and an unmaintained
permission set fails open in practice because people grant broadly to unblock.

**Defer the matrix and keep RLS as the only boundary.** What happens today, and it was survivable
while every caller was a browser driven by a person. It stops being survivable now that a token can
hold the credential: RLS says which rows you may see and nothing about whether you may dispatch,
merge or deploy.
