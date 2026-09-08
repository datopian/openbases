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
nothing.

The other seven need no skipping at all, and the first version of this document got the reason wrong.
It said they mix schema with seeding and are guarded by `INSERT … SELECT … WHERE`. They are not
guarded: their `INSERT` statements sit inside `CREATE FUNCTION` bodies, so they run when the function
is called and never when the migration is applied. `0084` registers a rig, `0057` a beads graph. The
measured result was right and the explanation was wrong — which matters, because the test protecting
this invariant has to strip dollar-quoted bodies to tell the two apart, and a version that matched
SQL text reported all seven as violations.

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

### Stage 1 — a tenant-neutral install, plus the test that keeps it one — **DONE, and corrected**

**The first version of stage 1 was wrong in both directions, and stage 2 found it.** Recorded in full
because the errors are more instructive than the feature.

`0027_execution_registry.sql` and `0052_register_source.sql` were in the manifest and seed nothing at
all: they define functions. `-fresh` skipped them, so every fresh install was missing
`system_register_execution_node`, `system_register_execution_cell`, `system_attach_project_to_cell`,
`system_record_usage`, `system_register_source`, `system_record_source_acl`,
`system_pending_receipts` and `system_mark_receipt_processed` — an install that came up reporting
itself clean and could not register a node, which is to say could not run an agent.

The test meant to catch that matched `CREATE FUNCTION` and these say `CREATE OR REPLACE FUNCTION`.
Nothing else looked, because every assertion was about rows.

`0018_credential_registry_seed.sql` and `0028_cost_import_credential.sql` were MISSING from the
manifest and seed `credential_registry` at the top level, ten Datopian addresses among them. The
assertion listed the sixteen tables it expected to be empty and that table was not one of them, so
"every tenant table is empty" was measured against an incomplete list.

Both were found by cross-checking the manifest against `scripts/disclosure_baseline.txt`, which
named different files. Two guards disagreeing was worth more than either agreeing with itself, so
that comparison is now a check: `check_disclosure.py` fails when the two lists diverge, before it
looks at its baseline, and no baseline entry can silence it.

Three further corrections came out of the same pass:

- Both guards stripped `DO $$ … $$` blocks along with function bodies. A DO block **executes**, so an
  insert inside one is a seed — `0080_restructure_projects.sql` seeds through one. Both now keep DO
  blocks and strip only function bodies.
- The empty-table assertion enumerates what may be populated (`roles`, `role_permissions`,
  `schema_migrations`) and requires everything else to be empty, rather than listing what may not be.
  It is the only version that also covers the table somebody adds next year.
- The assertion now checks that named functions **exist**, which nothing did before. Verified by
  dropping two of them: it fails and names them.

The baseline had also silently absorbed 45 false positives from the original rule. `check_disclosure.py`
now reports entries that no longer match anything, because a baseline that only grows stops
describing anything.

#### What stage 1 delivers, as corrected

`workgraph-migrate -fresh` reads `db/tenant_seeds.txt`, records those thirteen
as applied without running them, and refuses any database with a migration already applied. Verified
end to end against a real PostgreSQL with the real binary: thirteen recorded without running,
seventy-six applied, the assertion passing, and a second `-fresh` on the same database refused with
`needs an empty database, but 89 migration(s) are already applied`.

The assertion is `test/integration/fresh_install_has_no_tenant.sql`, and it was checked against a
NORMAL install to prove it is not vacuous: it fails there, naming `organisations holds 1 row`,
`users holds 8 row`, `projects holds 13 row`. CI runs both directions in the `database` job.

Three unit tests in `cmd/migrate` guard the manifest itself: every name in it is a real migration, no
file in it carries DDL, and — the important one — no migration OUTSIDE it seeds a record. Each was
checked by making it fail on purpose.

The original text follows.


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

### Stage 2 — creating the tenant — **DONE**

Delivered 2026-09-08. `wg-init` (`cmd/init`, cross-compiled by `make build-linux`) creates the
organisation, the first administrator and their `organisation_admin` grant:

```bash
wg-init -org acme -org-name "Acme Ltd" \
        -admin-email ops@acme.example -admin-name "Dana Ops"
```

It runs at install time on a host with database access, like `wg-registry`, and writes through
`system_bootstrap_organisation` (migration `0091`) — SECURITY DEFINER, because at bootstrap there is
no app user for RLS to check, by definition. Superuser would also work and is worse: it would put a
second unconstrained write path into the install story.

Three properties worth stating:

- **It refuses a second organisation.** One company per deployment is the documented stance, and a
  bootstrap that could quietly add a tenant would contradict it.
- **It is idempotent.** A repeat run reports `already_initialised` and changes nothing. A deploy-time
  tool people are afraid to run twice is one they run once, wrongly, and then patch by hand.
- **It creates no login identity, and none can be created.** A Cloudflare Access subject is issued by
  the provider on first sign-in and linked to a user by email address then. So the address given here
  has to be the one the provider asserts — otherwise the administrator is refused and the deployment
  has nobody who can fix it. The tool says this on every successful run, and CI asserts that it does.

`granted_by` on the bootstrap grant is NULL, deliberately: nobody granted it, the install did.
Naming the new administrator as their own granter would write the thing `AGENTS.md` rule 9 forbids
into the audit trail on day one.

Flags rather than prompts, which is a deliberate narrowing of what this document originally promised.
This step sits in an install script beside the migration, and a tool that stops to ask cannot run
unattended; an incomplete invocation refuses and names the missing flag.

The original text follows.


With no seed, a fresh install has no organisation and nobody can sign in. `wg init` should take an
organisation name and a first administrator, create the org, the user and the `organisation_admin`
grant, and stop. Interactive, or from a file for unattended installs.

This is the seam the guardrail already assumes exists: records about the real world belong in the
deployment's database, put there by an install step, not by a migration.

### Stage 3 — configuration that is ours — **DONE**

Delivered 2026-09-08, in two halves.

**`infra/tofu/envs/example/`** is a copy of `envs/staging` with our values removed, plus a README
naming what you need before starting (a Cloudflare zone, an R2 bucket for state, a Hetzner project,
a GitHub App, your own age key). It carries `terraform.tfvars.example` and `backend.hcl.example` and
deliberately no `terraform.tfvars`, so `tofu apply` there refuses until somebody makes the file on
purpose. `infra/ansible/inventory/example.yml` and `group_vars/example.yml.disabled` cover the
Ansible side — the execution cells especially, whose committed names are the clients they isolate.

`check_infra.py` keeps it honest: it fails if a variable is declared without appearing in the
example, if a stray `terraform.tfvars` appears, or if one of our values reappears in a config file
there. All three verified by making them fail. It scans configuration only — the README names
Datopian on purpose, because "these secrets are encrypted to our keys, create your own" is the
sentence that stops someone trying to use them.

That check found a leftover the copy would otherwise have shipped: `ai_gateway_store_id` **defaulted
to Datopian's AI Gateway log store**, so a third party's first plan would have pointed at our
resource. A default is the easiest kind of leftover to miss, because nothing about the file looks
filled in. The example now has no default for it.

**Workspace ingestion is off unless declared.** OpenTofu already gated the `google-events` module on
`google_project_id` being non-empty, and Ansible already enabled the workspace timer only with a
subject and a topic — so the infrastructure half existed. What was missing was that the code treated
absence as breakage, correctly for us and wrongly for anybody else:

- `internal/workspace`'s reconciler refuses an empty source list rather than deleting every
  subscription, because it cannot tell "never configured" from "configuration disappeared". Right,
  and it made `workgraph-workspace.service` fail on every run of a deployment that simply does not
  use Drive and Meet. `workspaced` now takes `-ingest` (from `WG_WORKSPACE_INGEST`) and exits 0 with
  a clear line when it is off.
- The monitor reported "no Google Workspace source is allow-listed" as a failure. It now takes the
  declaration through `Collector.WorkspaceIngest`, alongside `ExpectCells` and `Gateways` where
  declared expectations already live, and reports ingestion as off — still reporting, rather than the
  check disappearing, because a check that vanishes leaves nothing to distinguish "deliberately off"
  from "nobody runs this any more".

Ansible derives that flag from the two settings that already decide whether the timer runs, rather
than adding a second switch: two switches for one fact is how a deployment ends up with the timer
correctly off and the monitor permanently red. Verified against the real staging inventory, where it
derives `true`, so Datopian's own alerting is unchanged.

The original text follows.


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
