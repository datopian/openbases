# Renaming the repository to `openbases`

Written 2026-09-08, before the rename. Every "does not break" below was checked against the live
system rather than reasoned about, and the check is given so it can be repeated.

## The rename is smaller than the word count suggests

`workgraph` appears everywhere, and almost none of it is the repository's name. OpenBases is the
platform; **Workgraph is the work base inside it** — the README already draws this line, and it is
what makes most of these strings correct as they stand.

So this is an identity change for one GitHub repository, not a find-and-replace.

### Tier 1 — the repository's identity. Change these.

| What | Scale |
|---|---|
| GitHub repository name `datopian/workgraph` | 1 |
| Go module path `github.com/datopian/workgraph` | 144 occurrences in 64 files |
| Prose references in docs, README, AGENTS, SECURITY | 81 files mention `datopian/workgraph` |
| `git remote` on this machine | 1 |
| The local clone directory `~/code/workgraph` | separate decision, see below |

### Tier 2 — shares the word, is not the repository. Leave alone.

These name the **work base**, the Postgres role, the config directory and the services. Renaming any
of them is a different project with its own risk, and none of it follows from renaming a repository.

| What | Occurrences |
|---|---|
| `workgraph_app` — the Postgres role every RLS policy names | 118 |
| `/etc/workgraph` — config directory on every node | 48 |
| `workgraph_build`, `workgraph_ssh`, `workgraph_credentials` | 14 |
| `/var/lib/workgraph` | 7 |
| `workgraph-{worker,reconcile,workspace}.service` and timers | 5 |
| the `workgraph` database, `wg-*` commands, `wg-` bead prefixes | throughout |

### Tier 3 — would cost more than it returns. Leave, deliberately.

- **`workgraph-tfstate-staging` / `-production`** R2 buckets. Renaming means migrating OpenTofu
  state, which is the one operation that can lose the mapping between config and live
  infrastructure. Not worth it for a name.
- **GCP project `datopian-workgraph-events`.** A project id is immutable. The service account
  `workgraph-events@datopian-workgraph-events.iam.gserviceaccount.com` is fixed with it, and it is
  named in `event_sources`, in `0045`, and in the Pub/Sub push configuration.
- **Cloudflare AI Gateway names** (`workgraph-staging-oss`, `-internal`, `-client`). Renaming
  re-points every agent's gateway URL and orphans the cost attribution already imported against them.
- **The GitHub App `datopian-workgraph`.** Its slug is independent of any repository name, and it is
  installed across the org.
- **`~/.config/datopian-workgraph/`** — the age key and credentials paths, on each operator's machine.
- **`datopian/workgraph-agent-sandbox`.** A separate repository, and the only `workgraph`-named thing
  with a live database row: registered by `0016_register_agent_sandbox.sql`, which is applied and
  therefore frozen. Renaming it needs a migration that edits data, which the rules otherwise forbid.

## What breaks: checked, not assumed

| Surface | Breaks? | How this was checked |
|---|---|---|
| Hetzner deploy | **No** | Nodes never clone. There is no `git clone` or `ansible.builtin.git` anywhere in `infra/`, and no `github.com/datopian/workgraph` in `infra/`, `scripts/` or `.github/`. Ansible pushes. |
| Cloudflare | **No** | Nothing in `infra/tofu/` references the repository. What matches `workgraph` there is resource naming (`workgraph-${var.environment}`), the state buckets and the GCP project — none of them the repo. |
| Self-hosted CI runners | **No** | They are **org-level**: `gh api orgs/datopian/actions/runners` returns 2, and `gh api repos/datopian/workgraph/actions/runners` returns none. Nothing is registered against this repository. |
| GitHub App install, webhooks, branch rules | **No** | Keyed by repository id, not name. |
| Workflows | **No** | Only `main-push-guard.yml` names the repo, via `${{ github.repository }}`, which resolves at run time. |
| `project_repositories` rows | **No** | `datopian/workgraph` is not registered. Checked against staging: 15 rows, and the only `workgraph`-named one is `workgraph-agent-sandbox`. |
| `git remote`, `gh` CLI | No, redirects | GitHub keeps a permanent redirect for clones, pushes and API calls. Update anyway; a redirect is a grace period, not a configuration. |
| Go module path | No, redirects | Same redirect. Correctness, not breakage. |
| **Local `core.hooksPath`** | **Yes, if the directory is renamed** | It is an absolute path: `/Users/anuarustayev/code/workgraph/.beads/hooks`. Renaming the directory leaves it pointing at nothing, and **git does not warn** — hooks simply stop running. This repository has already been through one silent hook failure; see `scripts/bootstrap.sh`. |

Nothing in the deployment or in Cloudflare has to change. That is the headline.

## Two decisions that are independent

1. **The GitHub repository name.** Renaming it costs nothing operationally.
2. **The local directory name.** Nothing requires this. If it is renamed, `core.hooksPath` and
   `~/.claude.json` both hold the old absolute path and must be repaired.

## Order of work

```bash
# 1. GitHub. Redirects are created automatically.
gh repo rename openbases --repo datopian/workgraph

# 2. This machine's remote.
git remote set-url origin git@github.com:datopian/openbases.git
git remote -v

# 3. Only if the local directory is renamed too:
mv ~/code/workgraph ~/code/openbases
cd ~/code/openbases
git config core.hooksPath "$PWD/.beads/hooks"    # was absolute and now wrong
bash scripts/bootstrap.sh                         # reinstalls the pre-push hook
grep WORKGRAPH "$(git config core.hooksPath)/pre-push"   # must print, or the hook is not installed
```

Then two pull requests, kept separate because one is mechanical and one needs reading:

**PR A — the module path.** Mechanical, and it touches 64 files, so it wants its own diff.

```bash
go mod edit -module github.com/datopian/openbases
git grep -l 'datopian/workgraph' -- '*.go' | xargs sed -i '' 's|datopian/workgraph|datopian/openbases|g'
gofmt -l ./cmd ./internal && go build ./... && go test ./...
```

**PR B — prose.** The 81 files that name the repository. Read the diff rather than piping it through
`sed`: many mentions are `workgraph-agent-sandbox`, the work base, or the Postgres role, and all
three must survive unchanged. `git grep -w` for the exact string, not a substring — a substring grep
already produced one wrong number in this repository's audit.

Do not touch `db/migrations/`. Those files are applied and frozen; `cmd/migrate` refuses a deploy
when an applied migration's checksum changes. Their prose keeps saying `workgraph`, and that is
correct — it is what the repository was called when they were written.

## Verify afterwards

```bash
make check                                    # includes the disclosure check
git push                                      # the pre-push hook must still fire
cd infra/ansible && ../../scripts/with_secrets.sh staging \
  ansible-playbook -i inventory/staging.yml site.yml --tags code
```

Then confirm the remote MCP server still connects, and that a dispatched bead still opens a pull
request — both cross the GitHub App, which is the one place a name could hide.

## Open questions for Datopian

- Rename `workgraph-agent-sandbox` as well? It needs a data-editing migration, so the answer is
  probably no, or not yet.
- Change the Go module path now, or live on the redirect? Nothing outside this repository imports it,
  so this is tidiness rather than compatibility.
- Rename the local clone directory, or leave it? Leaving it costs nothing and avoids the
  `core.hooksPath` trap entirely.
