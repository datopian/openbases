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
| Cloudflare Access policy configuration | outstanding | WP-B1 |
| Google API scopes, Pub/Sub resources, allow-listed sources, subscription renewal evidence | outstanding | WP-H1 |
| Security and cross-source isolation test results | outstanding | WP-I3 |
| Meeting-to-workgraph scenario results | outstanding | WP-H3 |
| Candidate review and durable-publication evidence | outstanding | WP-H4 |
| Context-pack permission and freshness tests | outstanding | WP-H5 |
| Self-improvement PR, evaluation, approval, before/after measurement | outstanding | WP-H6 |
| End-to-end scenario results (§19 scenarios 1–14) | outstanding | see below |
| Backup identifiers and restore timestamps, including evidence snapshots | outstanding | WP-I2 |
| Load-test results | outstanding | WP-I3 |
| Known limitations within the defined go-live scope | **partial** | **Accepted risk: `main` is not protected.** Branch protection and rulesets on private repositories require GitHub Team; the `datopian` organisation is on Free. Direct pushes to `main` cannot be prevented. Compensating controls: the `main-push-guard` workflow fails on any commit that did not arrive through a merged pull request, and `make bootstrap` installs a `pre-push` hook. See [ADR-0016](../adr/0016-interim-unenforced-main-protection.md) and blocker Bead `wg-8yv.30`. |
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
