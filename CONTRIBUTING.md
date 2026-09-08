# Contributing

This repository is the whole of OpenBases, and most of it was written by agents running inside it.
Every rule below exists because a run found the gap it closes. They are not style preferences.

## Before your first change

Read [`AGENTS.md`](AGENTS.md). It is written for agents and read by people, and it is the operative
document — where this file and `AGENTS.md` disagree, `AGENTS.md` is right and this is a bug.

```bash
make bootstrap                 # pinned toolchain, each artefact SHA-256 verified
export PATH="$PWD/.toolchain/bin:$PATH"
make check                     # exactly what CI runs
```

`make check` is the gate. Run it before you push, not after CI tells you.

### Install the hooks

`scripts/bootstrap.sh` installs the pre-push hook, which refuses a direct push to `main` and runs the
disclosure check. It installs into the hooks directory git actually reads — `core.hooksPath` when it
is set, which `bd` sets — and appends itself between markers so it coexists with the section beads
manages there.

```bash
git config core.hooksPath          # where the hook has to go
grep WORKGRAPH "$(git config core.hooksPath || echo .git/hooks)/pre-push"
```

If that greps nothing, the hook is not installed and pushes are unguarded locally. CI still checks.

## One bead, one branch, one pull request

Every change is attached to a bead — the work item, in the graph, that says why the change exists.

```bash
bd create "what this delivers" --type task --priority 2 \
  --description "why it is needed" --acceptance "how anyone can tell it is done"
git checkout -b bead/<id>-<short-slug>
```

The branch is `bead/<id>-<slug>`. The pull request references the bead. **The pull request says what
command you ran and what it printed** — not "tested locally". If you changed behaviour on a deployed
system, the evidence is from that system.

Close a bead with evidence, not an opinion. "Works now" is not a close; the output that shows it is.

## What we look for in review, in this order

1. **Does it actually work, and how do you know?** A test that passes for the wrong reason is worse
   than no test. Twice in one week a test written to confirm reasoning disproved it instead, and both
   times the reasoning was the thing that was wrong.
2. **Is the reasoning in the repository?** A comment explaining why a thing is the way it is, next to
   the thing. A decision that changes the shape of the system goes in [`docs/adr/`](docs/adr/). If
   the reason lives only in a pull request description, it is lost.
3. **Is it honest about what it does not do?** State the limit. A change that half-works and says so
   is more useful than one that appears complete.

## Rules that are not negotiable

- **Never commit a secret.** Secrets are SOPS-encrypted in `infra/secrets/`. `make check` runs a
  history scan; if it fires, do not force it through.
- **Never commit data about the real world.** This repository is public, and a client's name is not a
  credential, so the secret scanners do not catch it. A client project, a person, a meeting, a
  repository attachment: all of it belongs in the deployment's database, seeded by an idempotent
  bootstrap or through the API. **Never from a migration** — an applied migration can never be
  edited, so a name seeded there is permanent. Live Google identifiers (Meet space codes and ids,
  Drive ids) belong in `event_sources` or in deployment configuration.
  `scripts/check_disclosure.py` enforces this in CI and on pre-push. It has a baseline for what is
  already applied; `--update` is for immutable history and read-and-verified fixtures, not for making
  a finding go away.
- **Never publish unreviewed model extraction.** A model's output about the company becomes a
  *candidate*. A named human accepts it before it is a record.
- **Never widen a classification.** Not by an agent, not by a migration, not as a side effect.
- **Nothing approves itself.** Not an agent, not a service, not the person who wrote the change.
- **A migration is forward-only and never edited once applied.** `cmd/migrate` refuses when an applied
  migration's checksum changes. Write a new one.
- **Migrations must be idempotent.** `DROP ... IF EXISTS` before every `CREATE`; a function that
  changes arity drops its own old signature. Three deploys failed in one day on this before
  `scripts/check_function_signatures.py` existed to catch it.
- **Do not weaken a guard to make a change pass.** If a test or checker blocks you, either the change
  is wrong or the guard is — argue the second in writing, in the place the guard lives. Several guards
  here carry a recorded exemption and its reason; that is the mechanism, not a bypass.

## Tests

| Kind | Runs against | When |
|---|---|---|
| unit | nothing external | `make check` |
| contract (`test/contract`) | a real `bd` binary | `make check` |
| integration (`test/integration`) | a real PostgreSQL | CI's `database` job |
| acceptance (`test/acceptance`) | staging, or a real tool | CI, some by hand |

An integration test that reads an RLS-protected table must `SET ROLE workgraph_app` or record why it
does not — CI runs as a superuser, which bypasses row-level security entirely, so a visibility
assertion made as the owner cannot fail. `scripts/check_rls_tests.py` enforces this.

## If you are an agent reading this

Your tools are read, search, edit, `bd`, and `wg-browse`. You cannot run `git`; the branch, the commit
and the pull request are made for you after your run finishes. Edit the files, comment on your bead,
and close it when the work is done — not when you have described it.

If an acceptance criterion asks for something you cannot do, say so plainly and leave it open. Do not
report a criterion satisfied because everything else was.
