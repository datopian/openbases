# infra/

Infrastructure as code. Everything persistent in Hetzner, Cloudflare, and the Google integration
project is defined here. **Production drift is forbidden** (plan §16.5, ADR-0015).

```
infra/
  tofu/
    modules/            reusable modules: node, network, tunnel, access, r2, pubsub
    envs/staging/       staging root module and variables
    envs/production/    production root module and variables
  ansible/              host provisioning, hardening, execution-cell prerequisites
  cloudflare/           Access applications, policies, and Wrangler configuration
  google/               enabled APIs, Pub/Sub topic and subscription, service identities
```

## Rules

1. **Staging first.** Destroy and recreate staging from code before touching production
   (WP-B1 acceptance).
2. **No inbound public services.** Nodes sit behind a default-deny firewall. The origin is reached
   only through Cloudflare Tunnel, which connects outbound.
3. **No secret in state.** Remote state holds no secret values. Host secrets use SOPS with age.
4. **Apply is a protected action.** `infrastructure.apply` requires an approval bound to the exact
   plan artefact; `infrastructure.destroy` requires two approvers.
5. **Drift detection is scheduled.** An undocumented manual change creates an incident and is either
   reverted or committed immediately.

## Status

The provider configuration and variable contracts are established here so that WP-B1 adds resources
rather than inventing structure. No resources are declared yet: declaring them requires the Hetzner
API token, the Cloudflare API token and account and zone identifiers, and the Google Cloud project
— none of which are in this repository and none of which may be committed.
