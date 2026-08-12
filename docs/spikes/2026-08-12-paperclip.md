# Spike: Paperclip as the execution and governance layer

- **Date:** 2026-08-12
- **Bead:** wg-8yv.41
- **Outcome:** do not adopt — see [ADR-0017](../adr/0017-not-adopting-paperclip.md)
- **Subject:** [`paperclipai/paperclip`](https://github.com/paperclipai/paperclip) at `master`, cloned 2026-08-12

Findings are from reading the source, not the documentation. File and line references are to the
clone at the date above.

## Subject

| | |
|---|---|
| Licence | MIT |
| Stack | Node.js server, React UI, PostgreSQL via Drizzle |
| Size | ~2,400 TypeScript files, 128 MB checkout |
| Created | 2026-03-02 |
| Activity | pushed daily |
| Stars / forks | ~77,300 / ~14,300 |
| Adapters | claude-local, codex-local, cursor-local, cursor-cloud, gemini-local, grok-local, opencode-local, pi-local, hermes, hermes-gateway, openclaw-gateway |

It is a serious, well-engineered project. Invariants are documented and then enforced by static
checks; error messages carry structured remediation. None of what follows is a criticism of its
quality.

## Question 1 — does the approval model support digest binding (ADR-0009)?

**No.**

`packages/db/src/schema/approvals.ts`:

```ts
export const approvals = pgTable("approvals", {
  id: uuid("id").primaryKey().defaultRandom(),
  type: text("type").notNull(),
  requestedByAgentId: uuid("requested_by_agent_id").references(() => agents.id),
  requestedByUserId: text("requested_by_user_id"),
  status: text("status").notNull().default("pending"),
  payload: jsonb("payload").$type<Record<string, unknown>>().notNull(),
  decidedByUserId: text("decided_by_user_id"),
  decidedAt: timestamp("decided_at", { withTimezone: true }),
  ...
});
```

- No digest, hash, or checksum column. Grepping the approval services for
  `digest|sha256|checksum|contentHash` returns nothing.
- Decisions are **mutations**, not appends: `approvals.ts` does
  `db.update(approvals).set({ status, decidedByUserId, decisionNote, decidedAt })`. The previous
  decision is overwritten, so the decision history is not preserved on the record.
- The approvals service has **no self-approval check**. Both `requestedByUserId` and
  `decidedByUserId` exist, but nothing compares them, in code or as a constraint.

An approval therefore authorises an approval *record*, not a specific diff, plan artefact, or image
digest. ADR-0009 requires the opposite, and our `approval_requests.action_digest` plus the
append-only `approval_decisions` trigger enforce it at the database.

**Worth stealing.** A separate mechanism, `server/src/services/issue-review-policy.ts`, implements
per-issue review policies:

| Policy | Meaning |
|---|---|
| `anyone` | no restriction — **the default** |
| `human_only` | only an authenticated user may submit a verdict; an agent is refused |
| `not_creator` | the verdict must come from someone other than the writer who moved the issue into review |

`human_only` is exactly the property plan §14.4 needs — a model may never be the reviewer — and
`not_creator` is a self-approval guard. Both are opt-in per issue and default to `anyone`, so
neither is a default-safe posture, but both are good primitives.

## Question 2 — is the isolation strong enough for a restricted government client?

**Partially, and the gap is structural.**

Better than expected: `packages/adapter-utils/src/local-process-sandbox.ts` implements real
Bubblewrap spawn confinement.

```
bwrap --die-with-parent --new-session --unshare-pid --unshare-ipc --unshare-uts
      [--unshare-net]              # networkScope: "deny"
      [--ro-bind | --bind] ...     # filesystemScope: "workspace"
```

`networkScope: "allowlist"` runs the CLI in a private network namespace and injects
`HTTP_PROXY`/`HTTPS_PROXY`, so every provider hostname must be listed explicitly. Allowlist entries
must be exact hostnames; wildcards are rejected.

Four limitations matter for our use:

1. **Off by default.** `claude-local/src/index.ts` documents both scopes as *"Off by default."*
2. **Linux only.** `local-process-sandbox.ts:347` throws *"Local process filesystem and network
   scopes are currently supported only on Linux."*
3. **No uid separation.** There is no `--unshare-user`, no `setuid`, no `--uid`/`--gid`. Every agent
   runs as the Paperclip server's own OS user. ADR-0002 requires a dedicated Linux user per cell.
4. **Secrets are company-scoped.** `packages/db/src/schema/company_secrets.ts` has
   `scope: text("scope").notNull().default("company")` and an `ownerUserId`, with no project or
   agent scope. Within one company there is no secret boundary between projects.

The structural consequence: to isolate a restricted client engagement you would model it as a
separate Paperclip **company**. That buys isolation at the cost of the single cross-project
portfolio view, which is the premise of plan §1.2 and ADR-0001.

Paperclip's isolation unit is the company. Ours has to be the client engagement inside one company.
Those are different shapes, and no configuration reconciles them.

## Question 3 — can Beads and Gas Town sit behind the adapter interface?

**No, structurally.**

`packages/adapters/AUTHORING.md` states the **no-remote-git contract**:

> The local execution-workspace cwd is the only persistence boundary across runs. No adapter may
> depend on a git remote for cross-run state. … Never `git push` from adapter runtime code. Never
> assume the local worktree has any `git remote` configured.

It is enforced, not merely documented: `scripts/check-no-git-push.mjs` scans adapter and runtime
source and fails the `policy` CI job on any unapproved `git push`. An opt-in escape hatch exists
(`// paperclip:allow-git-push: <reason>`) for operator-configured paths.

This conflicts directly with plan §4.5 and WP-E3, where the flow is branch → tests → **open a pull
request** → webhooks → merge queue, and with ADR-0015, which makes that the deployment model. The
README is explicit that this is deliberate: *"Paperclip orchestrates work, not pull requests. Bring
your own review process."* and *"Not a code review tool."*

Two further conflicts:

- **Worktree ownership.** Paperclip resolves an execution workspace per heartbeat and owns the
  worktree lifecycle. Gas Town does the same. Two worktree managers over one repository is a real
  conflict.
- **Beads as the issue store.** Paperclip issues are PostgreSQL rows with atomic checkout, execution
  locks, and goal ancestry. Substituting Beads is a fork of the persistence layer, not a plugin.

## Incidental findings

- **Authentication.** `server/src/auth/better-auth.ts` configures `emailAndPassword`; no OIDC or
  SAML provider is present. Cloudflare Access could sit in front as a network gate, but the
  application would keep its own identity system. Plan §8.1 wants one authenticated identity mapped
  to an application user, so this would be additional work.
- **No knowledge layer.** The nearest things are `skills/para-memory-files` and a
  `plugin-llm-wiki/wiki-ingest` skill. There is no source registry, evidence snapshot, typed
  candidate, review gate, classification inheritance, or context pack. Six code matches for
  "meeting" across the repository.

## What we take

Recorded as design inputs on the relevant work packages, not as dependencies:

| Design | Where it lands |
|---|---|
| `human_only` and `not_creator` review policies | WP-F2 (wg-8yv.16), WP-H3 (wg-8yv.22) |
| Bubblewrap spawn confinement, additive to per-cell Linux users | WP-E1 (wg-8yv.12) |
| Budget scoping by company/agent/project/goal/issue/provider/model with hard stops | WP-E1, WP-I1 |
| Workspace finalize failure as a run-level error gating dependent work | WP-E3 (wg-8yv.14) |

## When to revisit

This decision should be reopened if Paperclip adds per-project secret scoping, uid separation
between agents, and a supported push/PR adapter path. Those three changes would remove all three
blockers, and the build-versus-adopt arithmetic would change materially.
