# Architecture

The implementation specification is the approved baseline v1.1, held in
[`datopian/company-workgraph`](https://github.com/datopian/company-workgraph) under `plan/`.

It is not duplicated here. A second copy would drift, and the plan is a frozen, checksummed
artefact — `plan/BUNDLE-SHA256SUMS.txt` verifies it, and the governance CI in that repository
asserts it on every pull request.

| Reference | Where |
|---|---|
| Plan v1.1 | `company-workgraph/plan/datopian-workgraph-production-plan.md` |
| Plan SHA-256 | `254f43575307b9cce8639c6d8050f94957bc8fe25723d0fe2699f60252c3270f` |
| Diagrams | `company-workgraph/plan/diagrams/` |
| Decisions | [`docs/adr/`](../adr/) |

Where implementation experience contradicts the plan, the resolution is a new ADR that supersedes
the relevant plan section — not an edit to the plan and not an undocumented divergence.
