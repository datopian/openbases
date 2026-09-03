# ADR-0028: The remote MCP server is authenticated by Cloudflare Access Managed OAuth, not by an OAuth server of ours

- **Status:** accepted
- **Date:** 2026-09-03
- **Bead:** wg-p4h.11
- **Plan reference:** §8.1, §12.3
- **Relates to:** [ADR-0025](0025-api-first-external-clients.md), [ADR-0019](0019-execution-node-authentication.md), [ADR-0026](0026-pubsub-push-authentication.md)

## Context

ADR-0025 decided that Workgraph is reachable from the tools people already use,
and shipped that as an HTTP API plus a stdio MCP server (`wg mcp`) authenticated
by a personal token pasted into an environment variable. The bet was that a
local process reaches the audience.

It does not reach the audience we actually have. People at Datopian work in
Claude Cowork, claude.ai, the Claude phone apps, Claude Code and Codex. Of
those, the pasted-token model reaches only the two that run on a laptop with a
shell:

- **Cowork's sandbox cannot install `wg` and cannot hold an environment
  variable.** That consumed most of a working session on 3 September. The
  workaround was to build an RFC 8628 device authorization flow (`wg-2w0`) so a
  token could be obtained without a browser in the sandbox — which made a
  credential *obtainable* and did not make Workgraph *reachable*, because the
  sandbox still has no `wg` binary to use it with.
- **A phone can do neither**, and no amount of device flow changes that.

The way those clients reach remote tools is a **custom connector to a remote MCP
server, authenticated by OAuth in the person's browser**. There is no second
path. So the question is not whether to support OAuth but who implements it.

`wg-p4h.11` was originally filed to implement it ourselves, and said why that
was the shape:

> Cloudflare Access does not provide that for a third-party client, so it is a
> real build: an authorisation server, consent, per-client registration,
> refresh, and revocation that is visible to the token's owner.

That sentence stopped being true in March 2026.

## Decision

**Expose `/mcp` on control-api, behind the existing Access application, with
Access Managed OAuth switched on. Implement no OAuth in Workgraph.**

Managed OAuth is a transport for the authentication that already exists, not a
second way in. Verified against the current documentation and against staging
before deciding:

- An unauthenticated non-browser client receives `401` with a
  `WWW-Authenticate` header naming Access's discovery metadata. On our
  application that header is, before Managed OAuth is enabled:

  ```
  www-authenticate: Cloudflare-Access
    resource_metadata="https://work-staging.openbases.com/.well-known/cloudflare-access-protected-resource/mcp"
  ```

  which is RFC 9728 protected-resource metadata, path-scoped to the route. The
  document it points at names the authorization server
  (`https://datopian.cloudflareaccess.com`) and the authentication methods
  available.

- The client opens the person's browser to the **normal Access login** — the
  same Google Workspace IdP, the same MFA, the same email allow-list policies
  as the interface. Nothing about who may log in changes.

- Access issues an **opaque** access token, not a JWT, and resolves it at the
  edge. The origin still receives `Cf-Access-Jwt-Assertion`, which
  `internal/authn` verifies unchanged. Clients must support RFC 8707 resource
  indicators.

- Enabled by `oauth_configuration.enabled = true` on the application, which the
  Terraform provider expresses as of 5.24.0 — so it is in Git rather than
  applied by hand.

Three properties follow, and they are the reason this is the right answer rather
than merely the cheap one:

**Revocation is where revocation already is.** Removing somebody from
`access_allowed_emails` and applying ends their tool calls on the next request,
because Access enforces the same policies against the OAuth token as against a
cookie session. A token we minted ourselves would need its own revocation story,
its own audit trail, and its own answer to "this person left on Friday".

**The origin trusts exactly one thing, and already did.** `/mcp` reads
`Cf-Access-Jwt-Assertion` and nothing else. Identity, row-level security, role
grants, rate limits and idempotency are unchanged, because a tool call
dispatches into the same authenticated `/v1` chain in process. There is no
second authorisation path to keep in step with the first — which is the failure
mode that makes bespoke auth expensive long after it is written.

**A personal bearer token is deliberately NOT accepted on `/mcp`**, even though
`/v1` accepts one. The point of a connector is a credential a person authorised
in a browser and an administrator can revoke in one place. Accepting a pasted
token on this path would reintroduce precisely what ADR-0025 could not reach,
and would do it in the place least visible to the person whose credential it is.

### What is curated stays curated

The tool set remains the six tools ADR-0025 chose, moved to `internal/mcp` and
shared by both transports. The tool list IS the prompt — it is what a model
reads before deciding — so a long one makes every decision worse rather than
more capable. Approvals, token minting and the execution node's own paths stay
absent for the reasons ADR-0025 gives; a tool that always fails teaches the
model to keep trying it.

The two tools that spend money keep `SPENDS MONEY` in their descriptions and are
annotated non-read-only and destructive, so clients prompt. Both halves matter:
the description persuades the model, the annotation persuades the application.

## Consequences

Hosted clients can reach Workgraph, which is the point. A person connects once
per client and their tool calls carry their own identity, so what they see
through a connector is what they see in the browser.

`wg mcp` keeps working and is now a thin adapter over the shared tool set, so
the laptop and the phone cannot be served different tools.

**Sessions are in memory, bounded, and lost on restart.** Clients reconnect.
Putting them in Postgres would make a transport detail durable, and the thing
worth keeping across a restart — who the person is — lives in the token.

**Dynamic client registration is enabled on Access's side**, for localhost and
loopback redirects. That is Cloudflare's registration endpoint, not ours;
nothing about it reaches the origin. It is what lets a command-line client with
no pre-registered `client_id` connect at all, and the MCP authorization spec
expects it.

**The hosted clients' redirect URI is not yet in the allow-list**, because
Anthropic does not publish it and a guess would either silently do nothing or
silently permit a redirect we did not mean. It is read from the first real
connection attempt and added then. Until it is, the command-line clients work
and the hosted ones may not — a limitation to observe rather than design around.

**Per-person budgets become the eventual answer to spend.** `wg-p4h.9` proposed
per-token spend caps, which made sense when a token was a thing an agent held. A
Managed OAuth identity is a person, so the useful cap is per person, and it is
not built here. Noted rather than done.

Staging only. Production waits for the go-live evidence.

## Alternatives considered

**Our own OAuth 2.1 authorization server with dynamic client registration**, the
original `wg-p4h.11` scope. Rejected because the premise it rested on is no
longer true, and because the cost is not the authorization endpoint — it is
consent, refresh, per-client registration, revocation visible to the token's
owner, and a second authorisation path that must agree with Access forever.
Access now does all of it, enforces our existing policies against the result,
and is the thing already in the request path. Building it would be choosing to
own a security boundary we can get for a configuration flag.

**An Access MCP server portal**, which fronts several MCP servers behind one
Access-authenticated endpoint. Worth revisiting when a second internal MCP
server exists; with one server it adds a component and a hop to solve a problem
we do not have.

**A second Access application scoped to `/mcp`.** Rejected: it would need its
own policies, its own audience, and its own session duration, and the interface
and the tools would then be able to disagree about who may log in. One
application, one policy set, one answer.

**Keeping the device flow as the answer.** It solves obtaining a credential
where there is no browser, and it does not solve reaching Workgraph where there
is no `wg`. The two are complementary rather than alternatives: `wg-2w0` stays
for sandboxes that do have a shell.
