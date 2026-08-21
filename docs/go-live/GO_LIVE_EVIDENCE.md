# Go-live evidence pack

Plan §24.4. The system is **not live** until every section below is complete and every acceptance
scenario in plan §19 passes in production or a production-identical environment.

> **Status: incomplete.** Phase A (repository and governance bootstrap) is done. Phases B to I are
> outstanding. Nothing in this document may be marked complete without a link to the evidence.

| Section | Status | Evidence |
|---|---|---|
| Deployed hostnames and environment identifiers | outstanding | WP-B1 |
| Infrastructure plan and apply records | outstanding | WP-B1 |
| Pinned versions and checksums | **complete** | [`versions.lock`](../../versions.lock) — gt v1.2.0, bd v1.0.4, dolt v2.0.7, checksums verified on install by `scripts/bootstrap.sh` |
| GitHub App permissions | outstanding | WP-D1 |
| Cloudflare Access policy configuration | **partial** | Zero Trust organisation applied from code: `datopian.cloudflareaccess.com`, session 24h, seat expiry 730h. `deny_unmatched_requests` is **false**, pinned — set true on 2026-08-12 it returned 403 across ~50 unrelated Datopian production zones ([incident](../incidents/2026-08-12-deny-unmatched-requests.md), `wg-8yv.47`); this row previously recorded it as true, which was wrong. Google Workspace IdP created, `apps_domain` datopian.com. Per-application audiences, path-scoped for the two cell endpoints. Outstanding: group consent (`wg-8yv.44`). |
| MFA enforcement | outstanding | **Delegated to Google Workspace** by decision `wg-8yv.42`, so it cannot be evidenced from Cloudflare configuration alone. Requires the Workspace 2FA enforcement evidence in `wg-8yv.46`, and ideally the AMR assertion rule in `wg-8yv.45`. |
| Google API scopes, Pub/Sub resources, allow-listed sources, subscription renewal evidence | outstanding | WP-H1 |
| Security and cross-source isolation test results | **partial** | [Threat model](threat-model.md), ten attack paths with evidence and eight stated gaps. Cross-project isolation at load: 10 users against 50 projects, **zero leaks**, 21,000 restricted work items invisible to a non-member ([`test/load/leak_test.sql`](../../test/load/leak_test.sql)). Under concurrency: **zero violations**, including 250 goroutines over 5 connections ([`cmd/loadtest`](../../cmd/loadtest)). Cell isolation 8/8 including `hidepid` ([`test/acceptance/cell_isolation.sh`](../../test/acceptance/cell_isolation.sh)). Resource limits enforced and measured ([`test/acceptance/resource_limits.sh`](../../test/acceptance/resource_limits.sh)). Outstanding: `wg-8yv.50` (the RLS integration suite runs as superuser and cannot fail for the right reason), `wg-8yv.45` (MFA not asserted in policy). |
| Meeting-to-workgraph scenario results | outstanding | WP-H3 |
| Candidate review and durable-publication evidence | outstanding | WP-H4 |
| Context-pack permission and freshness tests | outstanding | WP-H5 |
| Self-improvement PR, evaluation, approval, before/after measurement | outstanding | WP-H6 |
| End-to-end scenario results (§19 scenarios 1–14) | outstanding | see below |
| Backup identifiers and restore timestamps, including evidence snapshots | outstanding | WP-I2 |
| Load-test results | **partial** | Target reached: 50 projects, 105,000 work items, 5,000 open, 60 users ([`test/load/`](../../test/load/)). Query latency: project-scoped 19.9 ms, paged 22.7 ms, portfolio 375 ms, **unfiltered 7.6 s** — RLS costs ~78 µs per candidate row, so cost scales with rows the planner cannot exclude (`wg-1ng`; no endpoint reaches the unfiltered path today). Concurrency at target: p50 542 ms, p95 947 ms, zero errors. Event ingestion at 100/min: 200 deliveries, all accepted and stored, worst 48 ms. 24-hour soak: **in progress**. |
| Known limitations within the defined go-live scope | **partial** | **Permanently accepted risk: `main` is not protected.** Decided 2026-08-14; Datopian will not upgrade to GitHub Team. Branch protection and rulesets on private repositories require GitHub Team; the `datopian` organisation is on Free. Direct pushes to `main` cannot be prevented. Compensating controls: the `main-push-guard` workflow fails on any commit that did not arrive through a merged pull request, and `make bootstrap` installs a `pre-push` hook. See [ADR-0016](../adr/0016-interim-unenforced-main-protection.md) and blocker Bead `wg-8yv.30`. |
| Rollback instructions | outstanding | WP-B1, WP-I2 |
| Named operational owners | outstanding | WP-I4 |

## Acceptance scenarios (plan §19)

| # | Scenario | Status |
|---|---|---|
| 1 | Role-aware portfolio | outstanding |
| 2 | Decision unblocks work | outstanding |
| 3 | Agent completes a real change | outstanding |
| 4 | Agent crash and recovery | outstanding |
| 5 | Protected merge or deploy | outstanding |
| 6 | Marketing workflow | outstanding |
| 7 | Client isolation | outstanding |
| 8 | User removal | outstanding |
| 9 | Disaster recovery | outstanding |
| 10 | Upgrade safety | outstanding |
| 11 | Meeting-to-workgraph | outstanding |
| 12 | Knowledge isolation and lifecycle | outstanding |
| 13 | Missed event recovery | outstanding |
| 14 | Governed self-improvement | outstanding |

## Sign-off

Leadership signs this pack only when every row above is complete with linked evidence.

| Role | Name | Date | Signature reference |
|---|---|---|---|
| Technical owner | | | |
| Leadership | | | |
