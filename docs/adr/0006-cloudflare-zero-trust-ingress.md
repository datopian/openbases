# ADR-0006: Cloudflare zero-trust ingress

- **Status:** accepted
- **Date:** 2026-08-10
- **Plan reference:** §8.1, §11.2, §17.3

## Context

The control plane holds client-restricted data. Exposing it on a public IP with an
application-managed login means owning session security, MFA, brute-force protection, and instant
revocation — and leaving an origin reachable from the Internet.

## Decision

Cloudflare Access is the external authentication layer, reached over Cloudflare Tunnel. The origin
has no inbound public services; the tunnel connects outbound only. Access enforces MFA. Break-glass
SSH uses Access for Infrastructure with short-lived certificates and command logging.

The application **validates the Access JWT itself** against the team's public keys. It does not
trust `Cf-Access-*` request headers, which anything that reaches the origin could forge.

## Consequences

- Removing a user from the Access group removes their access immediately, everywhere.
- Cloudflare becomes a hard dependency for reaching the application. A tunnel or Access outage is a
  documented runbook, and break-glass exists for it.
- Local development needs a static authenticator. It is confined to `authn.StaticAuthenticator` and
  the production authenticator fails closed, so an incomplete build denies rather than admits.

## Alternatives considered

**Application-managed sessions on a public origin.** Rejected: it puts a database holding client
data behind code we would have to harden ourselves, and leaves the origin scannable.

**Trusting `Cf-Access-Authenticated-User-Email`.** Rejected: header trust turns any request that
bypasses the tunnel into full authentication. The unit test for this is deliberately explicit.
