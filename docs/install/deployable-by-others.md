# Making OpenBases deployable by another company

Written 2026-09-08, immediately after the repository went public. Everything measured below was
measured by running it against a scratch database on the staging control node, not inferred from
reading the SQL. The scratch databases were dropped afterwards.

## Where it stands: another company cannot use this yet

A fresh `workgraph-migrate` against an empty database succeeds — all 89 migrations apply, no errors.
What it produces is Datopian:

| | count | what |
|---|---|---|
| organisations | 1 | `datopian`, and it is the only one |
| users | 8 | eight named employees, by work email |
| role_grants | 6 | including `organisation_admin` and `executive`, held by those addresses |
| projects | 13 | `bizdev`, `portaljs`, `datahub`, `sre`, … and the client engagements **`nged`** and **`cdt`** |
| portfolios | 4 | `oss`, `product`, `client`, `internal` |
| project_repositories | 13 | `datopian/portaljs`, `datopian/nged`, `ckan/ckan`, `flowershow/flowershow`, … |
| project_memberships | 40 | who is on what |
| event_sources | 5 | three Datopian shared drives, a client kick-off, the internal team sync |

So this is not a licensing or documentation gap. Install it today and the first screen shows another
company's clients, staff and repositories, with our people holding the admin grants. **This is the
concrete cost of the eight addresses**: they are not merely mentioned in the source, they are
`INSERT`ed as users on every deployment anybody ever makes.

It is not a route into Datopian's systems — authentication is each deployment's own Cloudflare Access
and IdP, so nobody signs in with those addresses on somebody else's install. It is worse as a product
problem than as a security one.

## Why this cannot be fixed by editing the migrations

`cmd/migrate/main.go:99` refuses any deploy whose applied migration no longer matches its recorded
SHA-256 — *"an applied migration must never be edited — write a new one"*. Twenty migrations seed
this data and all twenty are applied on Datopian's own databases. Editing them breaks every existing
deployment; deleting the rows in a new migration would delete Datopian's live data.

The fix therefore has to change what a **fresh** install runs, without touching the files.

## The mechanism, and it is tested

Thirteen of the twenty are **pure seed**: they contain no DDL, so skipping the whole file costs
nothing. The other seven mix schema with seeding and cannot be skipped — but they do not need to be,
because every one of their inserts is an `INSERT … SELECT … WHERE`, which finds nothing once the
organisation is absent.

Verified by pre-recording those thirteen as applied in `schema_migrations` — exactly what a
`--fresh` flag would do — and then migrating:

```
migrate exit: 0   applied: 76
organisations 0   users 0   projects 0   portfolios 0   repositories 0
memberships 0     role_grants 0          event_sources 0
work_refs 0       beads_databases 0      execution_rigs 0
roles 9   <-- survives, correctly: the permission model is schema, not tenant data
```

Zero rows about anybody, and the nine role definitions intact. That is the whole mechanism, and it
needs no change to a single migration.

## What to build

### Stage 1 — a tenant-neutral install, plus the test that keeps it one

`workgraph-migrate --fresh`, reading a manifest of the thirteen (`db/migrations/TENANT_SEEDS`) and
recording them as applied without executing them.

It must **refuse unless the database is empty**. A `--fresh` that could run against a populated
database is a way to skip a migration on a live deployment, which is the one thing `cmd/migrate`
exists to prevent. Datopian's databases need no special handling: those migrations are already
applied there, so nothing changes.

Then the part that makes this stay true: **a CI test that runs a fresh install and asserts every
tenant table is empty.** Without it, the next seed migration silently reintroduces the problem, and
nobody notices until an outside deployment does. `scripts/check_disclosure.py` already refuses a
*new* seeding migration; this closes the same gap from the other end, against what is already there.

### Stage 2 — creating the tenant

With no seed, a fresh install has no organisation and nobody can sign in. `wg init` should take an
organisation name and a first administrator, create the org, the user and the `organisation_admin`
grant, and stop. Interactive, or from a file for unattended installs.

This is the seam the guardrail already assumes exists: records about the real world belong in the
deployment's database, put there by an install step, not by a migration.

### Stage 3 — configuration that is ours

The infrastructure is in better shape than the database. OpenTofu already takes hostnames, the
Cloudflare account, the Workspace domain and the server types as variables in
`envs/*/terraform.tfvars`; a third party copies an environment directory and fills it in. What is
missing is an `envs/example/` with placeholders, so the only worked example is not Datopian's.

What genuinely carries our values and needs an example plus a note:

| Where | Ours | For a third party |
|---|---|---|
| `infra/tofu/envs/{staging,production}/terraform.tfvars` | `work-staging.openbases.com`, our Cloudflare account, `datopian.com` as the Workspace domain | copy to their own env directory |
| `infra/ansible/inventory/*.yml` | `ssh-staging.openbases.com` | their own hostnames |
| `infra/ansible/group_vars/all/registry.yml` | project-to-cell mapping, `client-nged`, `client-cdt` | their own projects, or none |
| `infra/ansible/group_vars/execution.yml` | the cell list, same names | their own cells |
| `internal/workspace/sources.json` | our three shared drives and the internal sync, by name | their own, or Workspace ingest left off |
| GitHub App | `datopian-workgraph` | their own App, id and private key |
| GCP project | `datopian-workgraph-events`, its service account and Pub/Sub topic | their own, and only if they want Workspace ingest |
| R2 state buckets | `workgraph-tfstate-*` | their own bucket |
| `infra/secrets/*.enc.yaml` | encrypted to our age keys | their own keys; the files are ours and are not usable by anyone else |

Workspace ingest (Drive and Meet) should be **optional at install**. It is the piece with the most
external setup — domain-wide delegation, a GCP project, Pub/Sub — and nothing else depends on it.
Today `cmd/workspaced` reads an embedded `sources.json` and a deployment with no drives should simply
not run it.

### Stage 4 — an install guide, and a smoke test that follows it

`docs/install/README.md`, written from a real run on a clean host, ending in a check that proves the
thing works: sign in, create a project, dispatch a bead, see a pull request. A guide nobody has
followed end to end is a guess.

## What not to promise

**One company per deployment.** The schema has `organisations` and the RLS policies are written
around it, but cells, rigs, the bead prefixes and the Cloudflare Access applications all assume a
single tenant. Multi-tenancy is a much larger piece of work and nothing here needs it. Say so plainly
in the install guide rather than leaving people to discover it.

## Order, and what each stage costs

1. **Stage 1** is small and worth doing first: a manifest, a flag, and a test. The mechanism is
   already proven, so this is mostly writing the guard and the test.
2. **Stage 2** is a new command over existing domain code.
3. **Stage 3** is mostly moving values into an example and documenting; the parameters exist.
4. **Stage 4** costs a clean host and a careful afternoon, and is what turns the rest into something
   an outsider can actually use.

Stages 1 and 2 together are what make the repository honest about being open source. Until then
"install it yourself" means "install our company".

## Open questions

- Single-tenant only, confirmed? Recommended, and it should be stated in the install guide.
- Should Workspace ingest be off by default? Recommended: it is the largest external setup and
  nothing depends on it.
- Do the thirteen seed migrations stay in the repository once `--fresh` skips them? Recommended yes:
  they are applied history on our own databases and `cmd/migrate` verifies them there.
