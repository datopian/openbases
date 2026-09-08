# Audit before making this repository public

**Date:** 8 September 2026 · **Scope:** every commit reachable from every ref, 581 commits.

Making a repository public exposes more than `main`. GitHub publishes every branch, every tag, and the
commits of every pull request — including ones whose branches were deleted. So the question is not
"is `main` clean" but "is everything a stranger could reach clean".

CI's own secret scan is bounded to `HEAD`'s ancestry, deliberately and for a good reason
(`scripts/secret_scan.sh` explains it: a persistent runner accumulates `refs/remotes/pull/N/merge`
forever, and a secret in a commit the repository no longer contains would fail every later job). That
bound is right for CI and wrong for this question, so this audit widened it.

## What was checked, and how

| # | Check | Scope | Result |
|---|---|---|---|
| 1 | gitleaks, working tree | 35.3 MB | clean |
| 2 | gitleaks, `main` ancestry | 242 commits | clean |
| 3 | **gitleaks, every ref** | **581 commits**, incl. 239 PR refs | **3 findings, all false positives** |
| 4 | Targeted grep for this system's own credential shapes | 581 commits | 3 hits, all false positives |
| 5 | Every version of every `infra/secrets/*` file | 13 versions | all encrypted |
| 6 | Blobs over 200 KB | all refs | 1, inspected below |
| 7 | gitleaks inside that archive | 539 extracted files | clean |
| 8 | Ingest and knowledge fixtures | — | synthetic; no real transcripts |

## The three gitleaks findings (all false positives)

None is on `main`; all are in commits reachable only from pull-request refs, which is exactly the class
this audit existed to find. All three are `generic-api-key`, which fires on any long random-looking
string.

- `internal/domain/resolver_test.go:48,50` (commit `bc28701b`, `pr/118`) — two **UUIDs**, in `UserID`
  and `TokenID` fields of a test fixture. A UUID is an identifier, not a credential.
- `internal/authn/spike_bearer_test.go:18` (commit `fa6226a2`, `pr/101`) — a 26-character constant
  named `spikeToken`, shaped `wgp_spike_…`. It contains the word "spike", so it was written by hand.
  **Verified rather than assumed:** its SHA-256 matches no row in `api_tokens`, so it was never a
  minted credential.

## The targeted grep (all false positives)

gitleaks has one ruleset; these are the shapes *this* system's real secrets take.
`cf-aig-authorization` bearer values, `WG_ACCESS_CLIENT_SECRET`, `ghp_`/`ghs_`/`github_pat_`,
`sk-ant-`, and a JSON `"private_key"` all returned **zero** commits. Three patterns matched, and each
matched a file that *describes* a secret rather than containing one:

- `test/integration/credential_registry.sql` — the literal `-----BEGIN RSA PRIVATE KEY-----`, a bare
  header with no key material, in a test asserting the registry **refuses** to store a PEM. The next
  line raises if it is accepted.
- `scripts/with_secrets.sh` — `export WG_DB_APP_PASSWORD="$(read_key db_app_password)"`, an assignment
  from SOPS.
- `.gitleaks.toml` — `AKIAIOSFODNN7EXAMPLE`, AWS's own documented example key, quoted in a comment
  explaining that planting it proves nothing because it is allowlisted upstream.

## The archive in `main`'s history

`_to_delete_wg-tracked.tgz`, 0.9 MB, a `bd` backup artefact that reached `main` in #180 and was removed
in #181. **The removal took it out of the tree, not out of the history**, so it is still published.

It matters because **gitleaks cannot see inside it**: the git scan reads the blob, and gzipped content
matches no pattern. A secret in there would have passed every scan above. It was therefore extracted
and scanned directly — 539 files, clean. `.claude/settings.json` inside it holds `bd` hooks and no
token, and the only `infra/secrets` file inside is the encrypted `staging.enc.yaml`.

So it is 0.9 MB of history bloat, not a leak. Removing it needs a history rewrite, which is a bigger
decision than it is worth for bloat alone — recorded here so nobody has to rediscover what it is.

## Not secrets, but disclosure — decisions for Datopian

None of this is a leak. All of it becomes public, and none of it is our call.

**These live in the repository itself, not only in the deployed database.** The seed migrations
create the real client projects as schema data, so the names ship in the source tree:

```sql
-- db/migrations/0009_pilot_seed.sql:73
('nged', 'NGED', 'Restricted client engagement', 'client', 'confidential', ...
```

- **Real client identifiers, by area \u2014 DECIDED 2026-09-08: publishable.** Datopian names clients in
  its own blog posts, so the repository revealing the same names discloses nothing new. No action.
  Recorded here so the question is not reopened by the next reader of this file.

  Detail: `nged`/`NGED` in 36 files (17 code and tests, 10 migrations,
  6 infra, 3 docs), `cdt`/`CDT` in 32, `roseville` in 12, `jackson` in 9 — as project slugs, cell
  names (`client-nged`, `client-cdt` in `infra/ansible/group_vars/`), test fixtures and ADR prose.
  Publishing reveals who Datopian works with, and in places what the work is. Client contracts may
  have something to say about that.
- **Live Google identifiers were in the source, and are now out of it.** The seed migrations carry
  two Meet space codes and three shared-drive IDs. None is a credential and Workspace enforces
  membership on every one, so publishing them grants nobody access \u2014 but they are operational, and
  they were also duplicated into live source that had no use for them.

  The literal values are deliberately NOT reproduced in this document. This file is published too,
  and an audit that lists what should not be published defeats itself. They are in
  `0045_seed_event_sources.sql`, `0048_seed_cdt_kickoff_space.sql`, `0050`, `0051`.

  **Removed from live source (this change).** `internal/workspace/sources.json` is `go:embed`-ed into
  the binary, so the drive IDs and the Meet space code shipped in the compiled artifact. Nothing read
  them: production calls only `VerifiedWants()` (`cmd/workspaced/main.go:99`), which returns event
  types, and the addressable identifiers come from the `event_sources` table. They are gone from that
  file, from the `VerifiedSources` struct, from the `lifecycle` test fixtures (synthetic ids assert a
  URL shape just as well) and from a runbook example.

  **Still in four applied migrations, and that is settled.** `cmd/migrate/main.go:99` refuses any
  deploy whose applied migration no longer matches its recorded SHA-256 \u2014 "an applied migration must
  never be edited \u2014 write a new one". Those four cannot be scrubbed without breaking every existing
  database, which puts them in the same bucket as git history: a record of what happened.

  **The recurring Meet code will be rotated instead**, which is the better fix anyway: rotation makes
  every remaining copy stale by construction, including the frozen migrations, without editing
  anything. The other code is a one-off client kick-off already in the past. Drive IDs cannot be
  rotated, but a drive ID grants nothing without membership.

  `0048` also records in prose that the CDT engagement was **pre-contract, with commercial terms**.
  That sentence, not the space code, is the sensitive half of that entry, and it is in a frozen
  migration.
- **Nineteen employee email addresses** appear in seeds, tests and runbooks, and eleven surname
  references (`Demenech`, `Okungbowa`, `Popova`, `Rubaj`) name who is accountable for which client.
- **Infrastructure hostnames** (`work-staging.openbases.com`, `ssh-staging…`, `ssh-exec-staging…`) are
  throughout. They have no inbound port and sit behind Access, so this is reconnaissance value rather
  than access — but it is a decision, not an accident.

### A correction to an earlier count

The first version of this audit reported `nged` in **127 files**. That number was wrong: it came from
a substring grep, and `nged` is a substring of **"changed"**. 120 of those 129 matches were `changed`
and `unchanged` in ordinary prose and code. The real figure is 36 files, above, counted with
`git grep -lw` against tracked files in both cases.

Two lessons worth keeping, because the mistake inflated a disclosure risk rather than hiding one:
short identifiers need `-w`, and `grep -i` with a negated class like `[^a-z]` does not reliably act
as a word boundary. Under zsh, also note that `set -- $var` does not word-split, so a loop built that
way silently searches for the whole string and reports zero.

## Conclusion

**No credential, key, token or password was found in any of the 581 commits.** Nothing here requires a
rotation or a history rewrite on security grounds.

What remained was not a security question, and is now largely answered. Client names are publishable
(Datopian names clients publicly anyway). The Google identifiers are out of live source, frozen in
four migrations, and the recurring Meet code is being rotated, which retires every remaining copy.
Open: the nineteen employee email addresses, and one evidence file discussed below.

## Reproducing this

```bash
git fetch origin '+refs/pull/*/head:refs/remotes/pr/*'   # what goes public, not just main
gitleaks git . --redact --log-opts="--all"               # 581 commits, not 242
```

And for any archive found in history, extract it and scan the contents — the git scan cannot.

Counting a short identifier needs `-w`, and both cases, against tracked files only:

```bash
{ git grep -lw nged; git grep -lw NGED; } | sort -u | wc -l   # 36, not the 127 a substring grep claims
```

## One file left as it is, on purpose

`docs/go-live/evidence/2026-08-31-workspace-subscriptions-staging.json` names the CDT kick-off space
in its record of the 2026-09-01 proof. It was left untouched. It is dated evidence that a
subscription discovered a real meeting without being told about it, and editing an evidence artefact
to look better in public is how evidence stops being worth keeping. The meeting is past and the code
is spent, so there is nothing live in it.
