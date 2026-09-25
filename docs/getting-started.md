# Getting started: run your first work through the MCP

This is the recommended way to use OpenBases day to day. You do not open a web
page and click buttons; you talk to a client you already use — Claude (desktop,
web, mobile), Claude Code, or Codex — and OpenBases' tools appear inside it over
[MCP](https://modelcontextprotocol.io). You describe the work in plain language,
your client calls the right tool, and the platform does the rest.

The loop, end to end, is five moves:

1. **Connect** the MCP server once.
2. **Create a project** (it needs two owners).
3. **Attach the repositories** the work touches.
4. **Plan and file the work** as beads — this is free, no agent runs.
5. **Dispatch** a bead — this spends money, an agent runs and opens a pull request.

Everything below shows both what to *say* to your client and the tool (and its
arguments) that runs underneath, so you can see what is happening. You rarely
type the arguments yourself — the model reads your intent and fills them in.

> **Host.** Examples use the Datopian staging host,
> `https://work-staging.openbases.com`. Substitute your own deployment's host.

---

## 0. Connect the MCP server (once)

The connector has no password and no pasted token: your client opens your
browser, you complete the normal login (Google Workspace, MFA, the same
allow-list as the web interface), and Cloudflare Access issues the token. What
you can see and do through the connector is exactly what you can see and do in
the browser — a connector is not an elevated path ([ADR-0028](adr/0028-remote-mcp-via-access-managed-oauth.md)).

**Claude Code:**

```bash
claude mcp add --transport http openbases https://work-staging.openbases.com/mcp
```

**Claude desktop / web / mobile / Cowork:** Settings → Connectors → **Add custom
connector** → URL `https://work-staging.openbases.com/mcp` → Save → **Connect**.

**Codex CLI:**

```bash
codex mcp add openbases --url https://work-staging.openbases.com/mcp
```

**On a laptop with a shell**, `wg mcp` serves the same tools over stdio, no
connector and no OAuth:

```bash
wg login            # once
codex mcp add openbases --command wg --args mcp
```

To confirm it worked, ask your client **"what needs me?"** — it calls
`workgraph_inbox` and should match the **Needs you** page in the browser.

Setup problems (the "Recommended" OAuth mode that does not work, hosted-client
redirect allow-lists, revoking access) are all in
[`docs/runbooks/connect-a-client.md`](runbooks/connect-a-client.md). Come back
here once "what needs me?" answers.

---

## 1. Create a project

A project is the unit of ownership and visibility. Its **slug is permanent** — it
appears in bead labels (`wg-project-<slug>`) and decides who can read the work —
so choose it deliberately.

Every project needs **two different people** as owners: a primary and a backup.
The registry refuses a project where one person holds both roles, so there is no
default the tool can invent. The primary owner defaults to whoever is asking; you
must name the backup.

> **Say:** "Create a project `jackson-open-data` called *Jackson Open Data*, with
> Daniela as the backup owner."

Underneath, that is `workgraph_project_create`:

| Argument | Required | Notes |
|---|---|---|
| `slug` | yes | short, lowercase, permanent (e.g. `jackson-open-data`) |
| `name` | yes | human-readable (e.g. `Jackson Open Data`) |
| `backup_owner` | yes | email of a **different person** from the primary owner |
| `primary_owner` | no | defaults to the caller |
| `portfolio` | no | slug of the portfolio it belongs under (e.g. `bizdev`) |
| `objective` | no | one sentence on what the project is for |
| `visibility` | no | `internal` (default), `public`, or `restricted` |
| `cell` | no | usually omitted; the deployment's shared cell (`oss`) runs it. **Required when `visibility` is `restricted`** |

### Changing owners, cell, or access later

Owners are set at creation. Changing them afterwards — or assigning a project to
a different execution cell, or granting a third person access — is **not** an MCP
tool; do it in the web admin on the project page (`work → the project → Manage
owners / Execution cell / Grant access`), or via the API:

```bash
# reassign owners (both, in one call; backup must differ from primary)
curl -X PATCH .../v1/projects/jackson-open-data -d '{"primary_owner":"a@x.com","backup_owner":"b@x.com"}'

# assign the project to an execution cell (needed before a repo becomes a rig)
curl -X PATCH .../v1/projects/jackson-open-data -d '{"cell":"oss"}'

# grant someone full access without displacing the owners (role defaults to project_lead)
curl -X POST .../v1/projects/jackson-open-data/members -d '{"email":"c@x.com"}'
```

These are kept off the MCP surface on purpose: they change who is accountable and
who can see the work, so the person doing it should read what they are doing.

---

## 2. Attach the repositories

Work runs against code. A cell provisions **one rig per attached repository**, and
a dispatch is routed to the rig holding the code a bead is about. A project with
no repositories cannot be dispatched to.

> **Say:** "Attach `datopian/portaljs` and `datopian/cloud.portaljs.com` to
> jackson-open-data."

Underneath, `workgraph_repositories_attach`:

| Argument | Notes |
|---|---|
| `slug` | the project's slug |
| `repositories` | list of `owner/name` — several at once; each is reported separately (`attached`, `already_attached`, or `taken`) |

An empty repository (no commits) cannot be checked out, so a first commit is
created for it and the result says so.

> **Note:** a repository only becomes a provisioned rig once the project is
> assigned to a cell that runs it. Internal and open-source projects use the
> deployment's shared cell by default, so this is automatic. If you set a custom
> cell, make sure the project is assigned to it (see *Changing … cell* above)
> before you expect the repo to be picked up.

---

## 3. Plan and file the work

You plan wherever you think best — in the chat with your client, in a doc, in your
head — and then file the finished plan in one call. **Filing costs nothing:** no
agent runs, it only writes the work graph.

A plan is a list of beads. Each bead carries a `ref` (your own stable name for it,
like `scaffold` or `deploy`), a title, acceptance criteria, and the `depends_on`
edges to other beads. The whole plan goes in one call because a plan is a graph;
filing it a bead at a time can half-fail, and a half-filed graph is worse than a
refused one.

> **Say:** "File this plan for jackson-open-data: (1) *scaffold* — set up the
> Next.js app, done when `npm run build` passes; (2) *catalog page* — list the
> datasets, depends on scaffold, done when the page renders the catalog from the
> API."

Underneath, `workgraph_beads_file` with `project` and a `beads` array; each bead:

| Field | Notes |
|---|---|
| `ref` | **required** — your name for the bead; the **upsert key** (stable across re-plans) |
| `title` | required to create; optional when closing/revising by `id` |
| `acceptance` | what "done" means, checkable — **write this**; without it an agent invents its own criteria and closes against those |
| `description` | the context an agent needs |
| `design` | how it should be done, if you have an opinion |
| `depends_on` | list of `ref`s in this plan, or existing bead ids, that must finish first |
| `type` | `task`, `feature`, `bug`, `chore` |
| `priority` | 0 (highest) to 4 (lowest); omit if you do not care |
| `id` | the id of an **existing** bead to revise or close (e.g. `msf8-gru`) |
| `status` | set to `closed` to close a bead |

Two things that make this safe to do often:

- **Re-filing the same plan revises those beads, it does not duplicate them** —
  the match is on `ref`, and a field you leave out is left alone, not cleared. So
  re-plan as much as you like.
- **To close or revise a bead you only know by id** (e.g. `msf8-gru`), give that
  bead an `id` and set `status: closed`. Without an `id` the filing matches on
  `ref` and would create a *new* bead — a common mistake.

The tool answers with each `ref` and the bead id it became, or a **job id** if the
node is busy — pass that to `workgraph_job` (say "how's that job going?") to see
it finish.

---

## 4. Dispatch a bead

This is the one that **spends money**: an agent runs against the bead in an
isolated cell, does the work, and the platform (not the agent) lands the result as
a pull request. Your client will prompt before running it — confirm work you did
not explicitly ask to run.

> **Say:** "Dispatch the scaffold bead" (or "dispatch `wg-abc`").

Underneath, `workgraph_dispatch`:

| Argument | Notes |
|---|---|
| `bead` | the bead id (e.g. `wg-abc`) |
| `rig` | optional — only needed if dispatch says several rigs hold this project's repositories, then name one of the rigs it listed |

Only dispatch beads whose dependencies are met — a bead still blocked by an
open `depends_on` is not ready. Ready beads for different projects can run in
parallel, up to the cell's concurrency ceiling.

**A refusal is a normal answer, not a fault.** If it says *"do not retry; this
needs a different credential or a person"*, your role or a policy/budget stopped
it — ask, do not retry. A budget refusal means the spend cap was hit. If it says
the authorisation expired, reconnect the connector.

---

## 5. Watch it and read the result

| Say | Tool | Shows |
|---|---|---|
| "how's that job going?" | `workgraph_job` | a queued file/dispatch job's progress, by the id it returned |
| "what's in flight?" | `workgraph_work_list` | what is running and what it cost |
| "show me bead wg-abc" | `workgraph_bead` | one bead: outcome, the agent's own words, model, cost, and the pull request |
| "what needs me?" | `workgraph_inbox` | decisions, risks and stalls ranked for you |
| "ask the chief of staff …" | `workgraph_ask` | the fixed question set over your portfolio |

When the agent finishes, `workgraph_bead` links the pull request. Review and merge
it like any other PR — the platform opened it, a human still lands it.

---

## The whole loop, once more

```
connect ──▶ create project ──▶ attach repos ──▶ file plan (free) ──▶ dispatch (spends) ──▶ read PR
              (two owners)      (one rig each)    (beads + deps)        (agent runs)         (bead → pull request)
```

That is the complete first run. From here it is the same three tools over and
over: **file** what you planned, **dispatch** what is ready, **read** what came
back — and re-file to re-plan whenever the shape of the work changes.

## The tool set

Ten tools, the same set over the hosted connector and over `wg mcp`, so nothing
is available in one client and missing in another:

**Read (free):** `workgraph_inbox`, `workgraph_ask`, `workgraph_work_list`,
`workgraph_bead`, `workgraph_job`, `workgraph_project_list`.
**Write (free):** `workgraph_beads_file`, `workgraph_project_create`,
`workgraph_repositories_attach`.
**Write (spends money):** `workgraph_dispatch`.

Some things are deliberately **not** tools, and stay API calls where the person
doing them reads what they are doing: detaching a repository (it withdraws the
route work travels on), setting a repository's check command (it is executed on
the execution node from a repo an agent can edit), and changing a project's
owners, cell, or membership (they decide accountability and who can read the
work). See [ADR-0025](adr/0025-api-first-external-clients.md) for why the tool set
is curated rather than the whole API.

## Related

- [`docs/runbooks/connect-a-client.md`](runbooks/connect-a-client.md) — full
  connection setup, every client, and troubleshooting
- [`docs/demo/staging-walkthrough.md`](demo/staging-walkthrough.md) — the same
  loop demonstrated through the web interface
- [ADR-0028](adr/0028-remote-mcp-via-access-managed-oauth.md) — why Access issues
  the token and OpenBases implements no OAuth
- [ADR-0003](adr/0003-beads-as-canonical-work-state.md) — why work is beads
