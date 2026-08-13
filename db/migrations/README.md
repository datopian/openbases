# db/migrations

Forward-only, numbered SQL migrations. Every migration is committed, reviewed, and applied by the
deployment pipeline — never by hand against a running database.

## Rules

1. **Backwards compatible.** A migration must be safe to apply while the previous application
   version is still serving (plan §16.2 deploys one service at a time).
2. **Destructive operations are protected actions.** Dropping or rewriting a column requires an
   approval and a pre-deploy backup, and is tested in staging first (plan §10.1, §16.4).
3. **Expand, migrate, contract.** Add the new shape, backfill, switch reads, then remove the old
   shape in a later release — never in one migration.
4. **No data edits.** A migration changes schema and performs backfills. It never fixes a business
   record; that is a work item with an audit trail.

## Migration set

| File | Work package | Contents |
|---|---|---|
| `0001_core.sql` | WP-C1 | Organisations, users, identities, roles and scoped grants, portfolios, functions, projects, memberships, repositories, execution nodes and cells, agent profiles, Beads database registry, work references and cross-graph links, the domain event spine, and the audit log. |
| `0002_knowledge.sql` | WP-H2, WP-H3 | Sources, snapshots, ACL entries, candidates, reviews, records, relations, context packs. |
| `0003_evaluation.sql` | WP-H6 | Agent runs and outcomes, human corrections, evaluation cases and runs, improvement proposals, prompt and formula versions. |
| `0004_attention.sql` | WP-F2 | Attention items and subscriptions, approval requests and decisions, narrative snapshots. |
| `0005_marketing.sql` | WP-G1 | Marketing packets and signals. |
| `0006_operations.sql` | WP-I1 | Usage records, budget limits, credential references, policy bundles. |
| `0007_projections.sql` | WP-H5 | `pgvector` extension and the rebuildable retrieval projection. |

Only `0001_core.sql` is present in the bootstrap. The rest land with their work packages; the table
inventory is fixed by plan §12.1 so the shape is not open to drift.

## Applying them

`cmd/migrate` applies pending migrations in lexical order, each in its own
transaction, and records name plus SHA-256 in `schema_migrations`.

```bash
make build
sudo -u postgres env WG_DATABASE_URL='postgres:///workgraph?host=/var/run/postgresql' \
  /usr/local/bin/workgraph-migrate
```

Two things about that command are deliberate.

**It runs as the database owner, not as `workgraph_app`.** PostgreSQL 15 and later revoke `CREATE`
on `public` from `PUBLIC`, so the application role cannot create tables — and should not be able to.
DDL and DML are different privileges; `0008_rls.sql` grants the application only what it needs.

**It connects over the local unix socket**, so no password crosses a network and no superuser
credential has to exist as a TCP login.

Editing an applied migration is refused: the recorded checksum would no longer match, which means
the schema on that database is not what the repository describes. Write a new migration instead.

## Row-level security

Tables holding project-scoped or classified data enable RLS and are read through a role that
carries the caller's user ID in a session setting. Application-layer checks are the first gate;
RLS is the backstop that survives a missed `WHERE` clause.
