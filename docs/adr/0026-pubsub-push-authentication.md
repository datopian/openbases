# ADR-0026: Pub/Sub push is authenticated by a Google-signed token, not by a bypass

- **Status:** accepted
- **Date:** 2026-08-30
- **Bead:** wg-8yv.20
- **Plan reference:** §14.2, §11.2
- **Relates to:** [ADR-0006](0006-cloudflare-zero-trust-ingress.md), [ADR-0013](0013-source-acl-inheritance.md)

## Context

Workspace Events reach us as Pub/Sub push deliveries to an endpoint on the
control API. Every path on that host is behind Cloudflare Access, and Google
cannot complete an Access challenge any more than GitHub can.

The repository already has one answer to that shape: the GitHub webhook path is
excluded from Access and the handler verifies an HMAC signature instead. It
works, and its own comment is uneasy about it —

> An HMAC signature is the only thing standing between that endpoint and anyone
> who learns its URL.

Doing the same again would double a mechanism whose weakness is already written
down. And the exposure is worse here: a Drive event names a file, a permission
change and an actor, in a tenant where the connector can read every file it is
told about.

## Decision

**Pub/Sub signs an OIDC token; the handler verifies it.** The push subscription
is configured with `oidcToken`, so Google attaches a signed JWT to every
delivery. The handler checks the signature against Google's public keys, the
audience against a value only we and Google know, and the issuer.

Access still excludes the path, because it must — but the bypass is no longer
the security boundary. It is a routing decision, and the boundary is a signature
we verify.

Three properties this has that an HMAC does not.

**The secret is asymmetric.** Verification needs Google's public keys, so the
control node holds nothing that could be used to forge a delivery. An HMAC key
can sign as well as verify, and it lives on both ends.

**The audience binds the token to this endpoint.** A token minted for another
service, or captured from another deployment, fails here. A shared HMAC key
protects a URL; an audience protects an endpoint.

**Rotation is Google's problem.** Their signing keys rotate on their schedule
and the handler follows, so there is no credential of ours to rotate, leak, or
forget to revoke.

### The body is not evidence

A verified token proves the request came from Google's Pub/Sub. It does not
prove the message contents are true, and the two are easy to conflate.

So the handler stores an immutable receipt and returns quickly, and everything
that acts on the contents happens afterwards, against a source it re-reads
itself through the Drive or Meet API. The event says *something changed here*;
what changed is read, not accepted.

That also makes the acknowledgement deadline honest. Pub/Sub redelivers what is
not acknowledged in time, and a handler that did the work inline would be
retried mid-work.

### Every delivery is deduplicated

Pub/Sub is at-least-once by design and says so. The receipt is keyed on the
message id, and a repeat is acknowledged without being processed again — the
same discipline `internal/githubapp` applies to `X-GitHub-Delivery` and
`internal/cost` to the gateway call id.

## Consequences

The push subscription cannot be created without knowing the audience, so the
audience is configuration and not a constant — it moves with the environment,
like the Access AUDs.

An unverifiable delivery is dropped with a 401 and recorded. Pub/Sub will retry
and then dead-letter it, which is the correct destination for traffic that
cannot be authenticated: it is either an attack or a misconfiguration, and both
deserve to be looked at rather than silently accepted or silently discarded.

The GitHub webhook stays on HMAC. Migrating it is not in scope here and is not
free — GitHub does not offer OIDC for webhooks — but the asymmetry between the
two paths is now deliberate rather than accidental.

## Alternatives considered

**An Access service token, as the cells use.** Rejected: Pub/Sub cannot present
one. It supports an OIDC token or nothing.

**A shared secret in the push URL.** Rejected. It is an HMAC with fewer
properties: the secret travels in a URL, which is logged by more things than a
header, and it authenticates the sender of a request rather than the request.

**mTLS.** Rejected as unavailable rather than as wrong: Cloudflare terminates
TLS, so the origin sees no client certificate.
