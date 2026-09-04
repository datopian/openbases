module "environment" {
  source = "../../modules/environment"

  environment  = "staging"
  location     = var.hcloud_location
  network_cidr = "10.10.0.0/16"
  subnet_cidr  = "10.10.1.0/24"

  # Staging runs an execution node so the isolation model in ADR-0002 is proven
  # on a box that can be destroyed, rather than first exercised on the node
  # holding a client's code.
  with_execution_node   = true
  execution_server_type = var.execution_server_type

  ai_gateway_store_id = var.ai_gateway_store_id

  cloudflare_account_id  = var.cloudflare_account_id
  cloudflare_zone_id     = var.cloudflare_zone_id
  hostname               = var.hostname
  api_hostname           = var.api_hostname
  ssh_hostname           = var.ssh_hostname
  ssh_hostname_execution = var.ssh_hostname_execution

  access_allowed_emails = var.access_allowed_emails

  # The hosted Claude clients' OAuth callback (wg-p4h.11, ADR-0028).
  #
  # Claude connects to a custom connector from Anthropic's cloud rather than
  # from the device, so ONE redirect URI serves claude.ai, the desktop app,
  # Cowork and the phones. The command-line clients need nothing here:
  # allow_any_on_localhost and allow_any_on_loopback in the module cover them,
  # and Claude Code declares http://localhost/callback and
  # http://127.0.0.1/callback with only the port configurable.
  #
  # Left EMPTY at first, on the grounds that a guess would either silently do
  # nothing or silently permit a redirect we did not mean. It is not a guess any
  # more, but it is not from Anthropic's own documentation either -- their help
  # article 404s at the URL the search index has -- so it is corroborated
  # instead by the registration endpoint's behaviour, which is testable:
  #
  #   $ curl -X POST https://datopian.cloudflareaccess.com/cdn-cgi/access/oauth/registration   #       -d '{"redirect_uris":["https://example.invalid/never-allowed"],...}'
  #   {"error":"invalid_client_metadata",
  #    "error_description":"redirect_uri is not allowed by the account configuration"}
  #
  # So the allow-list IS enforced, and a hosted client is refused at
  # REGISTRATION until its URI is here -- which is why claude.ai could not have
  # connected before this line existed, and why the failure would have looked
  # like a client bug rather than a configuration gap.
  #
  # If a real connection still fails after applying this, read the redirect_uri
  # the registration actually asked for from Zero Trust -> Logs -> Access and
  # replace this value with it. docs/runbooks/connect-a-client.md says how.
  # Domain-scoped rather than one exact path, after the exact path failed.
  #
  # https://claude.ai/api/mcp/auth_callback is a REAL endpoint -- it answers 400
  # to a bare GET rather than 404, and claude.com/api/mcp/auth_callback does 404
  # -- so the URI itself was right, it was applied, and a hosted client still
  # could not connect. That leaves the registration REQUEST rather than the
  # value: a client that submits several redirect_uris is refused outright if
  # any one of them is outside the list, and we cannot see which ones it sends.
  #
  # So the list stops trying to predict the path. Cloudflare's own documented
  # example is domain-scoped in exactly this way
  # (https://playground.ai.cloudflare.com/*), and `/*` matches all sub-paths.
  #
  # What this widens, stated plainly: an authorization code may now be
  # redirected to ANY path on claude.ai or claude.com rather than one. What
  # holds it: PKCE S256, which the authorization server advertises and requires,
  # so a code intercepted at another path on that domain is not redeemable
  # without the verifier; the client is registered to those domains and nowhere
  # else; and a token, however obtained, still resolves to the person who
  # completed an interactive Access login and is enforced against the same
  # policies as their browser session.
  #
  # claude.com as well as claude.ai because Anthropic is mid-rename --
  # support.anthropic.com now 301s to support.claude.com -- and a callback that
  # moves domain would fail exactly like this.
  #
  # DECIDED on 4 September: this stays domain-scoped. It was written as a
  # temporary widening to be narrowed once the Access log named the exact URI,
  # and that is no longer the plan -- so the note is here rather than left as an
  # instruction somebody follows later and breaks the connector with.
  #
  # Anthropic is mid-rename (support.anthropic.com 301s to support.claude.com),
  # so the callback path is the part most likely to move; pinning it buys
  # exactness in exchange for an outage nobody would attribute to this file.
  # The security argument does not depend on the path -- PKCE S256 is required
  # by the authorization server, the client is registered to these two domains
  # and nowhere else, and any token resolves to a person who completed an
  # interactive Access login and is enforced against their own policies.
  #
  # What is actually given up: if a code were intercepted at another path on
  # claude.ai or claude.com, this list would not be what stopped it. PKCE would.
  access_oauth_allowed_redirect_uris = [
    "https://claude.ai/*",
    "https://claude.com/*",
  ]

  ai_monthly_budget = var.ai_monthly_budget
  ai_budget_shares  = var.ai_budget_shares
  admin_ssh_key_ids = var.admin_ssh_key_ids
  admin_ssh_cidrs   = var.admin_ssh_cidrs
}

# Workspace Events delivery fabric (WP-H1, unblocked by wg-8yv.34).
#
# count rather than a commented-out block: the module is inert until someone
# sets google_project_id, and a plan with it unset is a plan with no Google
# resources in it at all. That keeps `tofu plan` honest for everyone who has no
# Google credentials, which is everyone until this is turned on.
module "google_events" {
  source = "../../modules/google-events"
  count  = var.google_project_id == "" ? 0 : 1

  project_id  = var.google_project_id
  region      = var.google_region
  environment = "staging"

  # Pub/Sub pushes to the control API, which is behind Cloudflare Access. The
  # endpoint must therefore be reachable by Google, which is a separate decision
  # from the one this module makes — see the runbook.
  push_endpoint = "https://${var.hostname}/v1/google/events"

  # The service account already used for the connector. Pub/Sub only needs an
  # identity to sign the OIDC token as; it grants nothing by being named here.
  push_service_account = "workgraph-events@${var.google_project_id}.iam.gserviceaccount.com"
}
