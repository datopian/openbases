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
| Google API scopes, Pub/Sub resources, allow-listed sources, subscription renewal evidence | **partial** | [`2026-08-31-workspace-subscriptions-staging.json`](evidence/2026-08-31-workspace-subscriptions-staging.json). Project `datopian-workgraph-events`, two read-only scopes granted to client ID 102065960434834149554, topic and OIDC push subscription applied. Four allow-listed sources — three shared drives and one recurring Meet space — each with one active subscription, renewed hourly by `workgraph-workspace.timer` inside a 24-hour window. **The full delivery chain is proven end to end**: a Drive change produced `file.v3.created` and `file.v3.trashed`, each pushed with a Google-signed OIDC token, verified by the receiver and attributed to the All shared drive. Six of seven acceptance criteria pass under [`scripts/accept_workspace_events.sh`](../../scripts/accept_workspace_events.sh): renewal keeps the subscription id, a second pass changes nothing, a revoked source is unsubscribed immediately and restored on return, a delivery from an unknown subscription resolves to no source. Two IAM bindings on the push identity are documented prerequisites rather than code, because OpenTofu authenticates as that same service account — both are recorded in the evidence file with the command that detects them. **Outstanding:** a Meet transcript event, which cannot be forced (the meeting is Mon–Thu and sometimes skipped or held with transcription off); and the delegation subject is a person's own address rather than a dedicated account (`wg-map`). |
| Security and cross-source isolation test results | **partial** | [Threat model](threat-model.md), ten attack paths with evidence and eight stated gaps. Cross-project isolation at load: 10 users against 50 projects, **zero leaks**, 21,000 restricted work items invisible to a non-member ([`test/load/leak_test.sql`](../../test/load/leak_test.sql)). Under concurrency: **zero violations**, including 250 goroutines over 5 connections ([`cmd/loadtest`](../../cmd/loadtest)). Cell isolation 8/8 including `hidepid` ([`test/acceptance/cell_isolation.sh`](../../test/acceptance/cell_isolation.sh)). Resource limits enforced and measured ([`test/acceptance/resource_limits.sh`](../../test/acceptance/resource_limits.sh)). Outstanding: `wg-8yv.50` (the RLS integration suite runs as superuser and cannot fail for the right reason), `wg-8yv.45` (MFA not asserted in policy). |
| Meeting-to-workgraph scenario results | outstanding | WP-H3 |
| Candidate review and durable-publication evidence | outstanding | WP-H4 |
| Context-pack permission and freshness tests | outstanding | WP-H5 |
| Self-improvement PR, evaluation, approval, before/after measurement | outstanding | WP-H6 |
| End-to-end scenario results (§19 scenarios 1–14) | outstanding | see below |
| Backup identifiers and restore timestamps, including evidence snapshots | outstanding | WP-I2 |
| Load-test results | **partial** | Target reached: 50 projects, 105,000 work items, 5,000 open, 60 users ([`test/load/`](../../test/load/)). Query latency: project-scoped 19.9 ms, paged 22.7 ms, portfolio 375 ms, **unfiltered 7.6 s** — RLS costs ~78 µs per candidate row, so cost scales with rows the planner cannot exclude (`wg-1ng`; no endpoint reaches the unfiltered path today). Concurrency at target: p50 542 ms, p95 947 ms, zero errors. Event ingestion at 100/min: 200 deliveries, all accepted and stored, worst 48 ms. 24-hour soak: **in progress**. |
| Known limitations within the defined go-live scope | **partial** | **Permanently accepted risk: `main` is not protected.** Decided 2026-08-14; Datopian will not upgrade to GitHub Team. Branch protection and rulesets on private repositories require GitHub Team; the `datopian` organisation is on Free. Direct pushes to `main` cannot be prevented. Compensating controls: the `main-push-guard` workflow fails on any commit that did not arrive through a merged pull request, and `make bootstrap` installs a `pre-push` hook. See [ADR-0016](../adr/0016-interim-unenforced-main-protection.md) and blocker Bead `wg-8yv.30`. **Accepted risk: the per-domain AI Gateway split is not a security boundary.** An AI Gateway token cannot be scoped to one gateway, so a token stolen from the OSS cell reaches all three (`wg-4r2`); and every gateway in the account writes to one shared log store — nine gateways on 2026-09-01, six of them unrelated Datopian projects (`wg-90f`). Both accepted 2026-09-01 with reasoning in the [threat model](threat-model.md). Mitigated where it is free: control-plane inference sends `cf-aig-collect-log-payload: false`, so its bodies are not stored. Client-domain AGENT bodies are stored deliberately, because for an agent run the prompt is the audit trail. **Conditional on the engagement permitting shared retention** — revisit before any contract that forbids it (`wg-3nv`). |
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
