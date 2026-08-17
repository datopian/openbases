# Incident 2026-08-12: an account-level Access setting blocked live production zones

- **Bead:** wg-8yv.47
- **Severity:** high — third-party production traffic, on zones with no relationship to Workgraph
- **Detected by:** a human, not monitoring. That is a finding in its own right.
- **Status:** resolved; guards in place; one action outstanding (WP-I1)

## What happened

`deny_unmatched_requests = true` was set on the account's Zero Trust organisation. HTTP 403 with
Cloudflare error 1050 then appeared across live zones on the shared Cloudflare account, including
`datopian.com`, `www.datopian.com`, `viderum.com`, `f11s.com` and `taskgraph.dev`.

Confirmed with both `curl` and a real browser, so it was not bot filtering. `taskgraph.dev` had been
fetched successfully earlier the same day, which established the before and after.

Applied and reverted in the same session. The exact window was not instrumented, because nothing was
watching these zones.

## Root cause

I reasoned that the setting could only affect hostnames covered by an Access application, and that
with zero applications configured the blast radius was nil. I said so explicitly and applied on that
basis.

The setting is account-scoped and its effect is broader than its name implies.

The error was **reasoning about intent instead of verifying behaviour**, on an account carrying
roughly fifty production zones that have nothing to do with Workgraph.

## Contributing factors

1. **Workgraph does not own this Cloudflare account.** It shares it with Datopian's production
   zones, so an account-level Zero Trust setting is not isolated to Workgraph. This was already
   understood for DNS — the deploy token is deliberately scoped to `openbases.com` precisely so it
   cannot touch `datopian.com` — but the same reasoning was not carried across to account-level
   Access settings.
2. **No synthetic check existed over the pre-existing zones**, so a person noticed before any
   monitoring did.
3. **The setting was bundled into a larger apply**, which made attribution slower than it needed to
   be.

## Resolution

Reverted through OpenTofu rather than the dashboard, so state and code stayed consistent. Verified
afterwards across 18 zones: 18 serving normally, 0 blocked.

## What is now in place

| Action | Where it lives | Status |
|---|---|---|
| `deny_unmatched_requests` cannot drift back to `true` | `PINNED_ACCOUNT_ATTRS` in `scripts/check_infra.py` — pinned to `false`, so a dashboard change is reverted on the next apply and an attempt to set it in code fails the check | done |
| An unreviewed account-level attribute fails the build | `REVIEWED_ACCOUNT_ATTRS` in `scripts/check_infra.py`. Adding a name is a claim that you have established what it does to traffic that is not ours | done |
| Smoke-check the shared zones after any account-level apply | `scripts/check_live_zones.sh`, `make live-zones` | done, **manual** |
| Continuous synthetic checks over the pre-existing zones | WP-I1 observability (wg-8yv.26) | **outstanding** |

The pinned-value guard is worth calling out as the more useful of the two. Omitting the attribute
would have left the dashboard free to set it; declaring it explicitly at the safe value means
Terraform reverts it rather than ignoring it.

## The rule this produces

**Treat every account-scoped Cloudflare setting as production-affecting during review, regardless of
what the setting is named, for as long as Workgraph shares this account.** Prefer a per-application
control over an account-level one wherever both exist.

And the general form, which is the part worth keeping: *a blast-radius argument is not evidence.*
The reasoning here was sound and the conclusion was wrong, because the premise — that the setting
only evaluates hostnames with an Access application — was never checked against behaviour.

## Not re-enabling

`deny_unmatched_requests` stays `false`. Re-enabling it requires first establishing exactly which
traffic it evaluates, in a way that does not require an outage to learn — a test account, or
documentation from Cloudflare that states the scope precisely.
