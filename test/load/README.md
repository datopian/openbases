# Load and isolation validation (WP-I3)

Runs against a **separate** database, `workgraph_load`, on the staging control node.

Separate deliberately: the load target is 50 projects and 105,000 work items, while
`test/integration/pilot_registry.sql` asserts exactly three projects and twelve repositories.
Seeding the load data where the pilot lives would break a test that is doing its job, and would
leave the staging interface showing fifty fictional projects.

It shares the PostgreSQL *instance* on purpose — criterion 3 is that agent workloads cannot take
down the control plane, and a load test on a different server would not exercise that at all.

```bash
scripts/load_env.sh create    # database, migrations, extensions
scripts/load_env.sh seed      # 50 projects, 105,000 work items, 5,000 open
scripts/load_env.sh status
scripts/load_env.sh drop
```

## What is measured

| file | criterion | result |
|---|---|---|
| `leak_test.sql` | 1 — no cross-project leak | **pass**, 10 sampled users against 50 projects; 21,000 restricted items invisible to a non-member |
| `query_latency.sql` | 2 — UI usable at load | project-scoped 20 ms, paged 23 ms, portfolio 375 ms, **unfiltered 7.6 s** |

## Two traps in writing these, both hit

**A leak test must assert positive visibility.** One that returns zero rows for everyone passes
trivially. `leak_test.sql` fails if any sampled user can see *nothing*, because every one of them is
a member of at least one project by construction.

**Ground truth must be captured before dropping privileges.** `projects`, `project_memberships` and
`role_grants` are themselves under row-level security, so building the "what should this user see"
baseline after `SET ROLE workgraph_app` filters the baseline too — the test then compares RLS
against RLS and calls every visible project a leak. The first version did exactly that and reported
41 leaks across 10 users while printing `checked 10 users against 0 projects`. The zero is what gave
it away.

It also has to run as `workgraph_app` and not the owner: `postgres` is a superuser, bypasses RLS
entirely, and would make every query return everything while reporting no leak.

## The latency finding

Row-level security on `work_refs` is `can_read_project_row(project_id)`, a `SECURITY DEFINER`
function that PostgreSQL calls once per candidate row. Whether that matters depends on whether the
planner can shrink the candidate set with an index first:

- **filtered by project** — index reduces candidates, ~20 ms, fine
- **unfiltered** — 105,000 function calls, **7.6 seconds**

No endpoint reaches the unfiltered path today. The only `work_refs` queries in the API filter by
organisation, beads database and bead id. So this is **latent, not live** — and it becomes live the
moment someone adds a cross-project work-item listing, which is a plausible next feature.

`work_refs` also has no index on `project_id` in the base schema. Fine at three projects; at fifty
every project-scoped query is a sequential scan. The seeder creates one, and a real deployment needs
it in a migration.

## Not done yet

Concurrency test (10 concurrent streams, 25 active agents), 100 GitHub events/minute freshness,
the 24-hour soak, dependency and container scans, the resource-limit test for criterion 3, and the
threat-model review.
