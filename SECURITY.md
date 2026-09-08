# Security

## Reporting a vulnerability

**Use [GitHub's private vulnerability reporting](https://github.com/datopian/workgraph/security/advisories/new).**
It keeps the report, the fix and the disclosure in one place, and it works without either of us
publishing an address.

There is deliberately no `security@` alias quoted here. Datopian has not published one for this
project, and an address in a SECURITY.md that nobody monitors is worse than no address at all — it
looks like a channel and silently is not. If private reporting is unavailable to you, contact any
Datopian maintainer through the address on their GitHub profile and ask for a private channel.

Please do not open a public issue for a vulnerability. Please do not test against
`work-staging.openbases.com` or any Datopian-operated host — the system runs real client work, and a
test against it is an incident for somebody who did not ask for one. Run it locally instead
(see the README's *Run it*), or describe what you would do and we will run it.

**What to expect.** An acknowledgement within three working days. Datopian is an eight-person company;
there is no security rota and no on-call, so a report arriving on a Friday evening is read on Monday.
If a fix is warranted we will agree a disclosure date with you, and credit you unless you would rather
not be.

**In scope:** anything in this repository, and the deployed staging system's exposed surface — the
Cloudflare Access boundary, `/v1`, `/mcp`, the GitHub webhook endpoint, the isolation between execution
cells, and the agent tool policy.

**Out of scope** and already known: everything recorded in
[`docs/go-live/threat-model.md`](docs/go-live/threat-model.md), which lists accepted risks and the
conditions that would change each. Read it before reporting — a finding already written down there is
a decision, not a discovery, though an argument that a decision is wrong is welcome.

## What this system does with credentials

Useful context for anyone assessing it, and a checklist of what a finding here would be worth:

- **No credential is committed.** Secrets live in SOPS-encrypted files in `infra/secrets/`, decrypted
  into one child process by `scripts/with_secrets.sh` and never into an interactive shell. Every version
  of every `.enc.yaml` in this repository's history is encrypted; that is checked, not assumed.
- **Neither node has an inbound port.** Both dial out through Cloudflare Tunnel. People reach the system
  through Cloudflare Access, which authenticates before a connection is made.
- **A cell never holds a git credential.** The dispatcher asks the control node for a token scoped to a
  single repository, valid for under an hour. The GitHub App private key can mint tokens for every
  installed repository, so it stays on the control node, where no agent code runs.
- **The GitHub webhook endpoint bypasses Access**, because GitHub cannot complete an Access challenge.
  An HMAC signature verified before parsing is the only thing protecting it. This is the endpoint we
  would most want a second opinion on.
- **Agents cannot run `git`, `curl` or a shell.** Their tools are read, search, edit, `bd`, and
  `wg-browse` — one command that resolves a host, refuses link-local and private addresses, and pins
  the resolution so a name cannot change between the check and the fetch.
- **A repository's own build command is executed on the execution node** when somebody opts that
  repository in, after an agent has been able to edit the code it comes from. It runs with no git
  credential helper, so nothing it starts can mint a token or push, and the cell's own credentials are
  stripped from its environment. The residual risk is stated in `internal/check`'s package comment
  rather than hidden: a script in a repository can still reach the network.

## Rotating a credential

[`docs/runbooks/`](docs/runbooks/) has the operator procedures. `credential_registry` records what
exists, who owns it, and who may revoke it — the registry refuses to store a credential's *value*, and
there is a test asserting that it does.
