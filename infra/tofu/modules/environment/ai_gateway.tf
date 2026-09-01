# ---------------------------------------------------------------------------
# Model access for execution cells (WP-E2, WP-E3)
# ---------------------------------------------------------------------------
#
# Agents reach a model through Cloudflare AI Gateway rather than holding a
# provider API key directly.
#
# The reason is the threat model, not convenience. Execution nodes run agent
# workloads on repositories the agents themselves modify, which is as close to
# untrusted as code on our own hardware gets. A stolen provider key is account
# access: unmetered, unattributed, and revocable only by rotating a credential
# every cell shares. A stolen gateway token is metered, logged, scoped to one
# security domain, and revocable on its own.
#
# One gateway per security domain — for attribution and blast-radius bounding,
# NOT for isolation. The distinction matters and was got wrong here first.
#
# Cloudflare cannot scope an AI Gateway token to one gateway: the Read, Run and
# Edit permissions are account-wide, unlike R2 which supports per-bucket
# scoping. Any token with Run reaches EVERY gateway in the account and can
# consume the provider keys stored on them. Confirmed on staging, where one
# token was accepted by all three gateways and each returned a model response.
#
# So a token stolen from an open-source execution node can spend the client
# domain's budget and forge which domain spent it. What the split does provide
# is a separate spend ceiling and separate analytics per domain. NOT separate
# logs — every gateway in the account writes to one shared store (wg-90f, and
# the store_id note below) — and NOT separate credentials, because an AI Gateway
# token cannot be scoped to one gateway and the same provider key is stored on
# all three (wg-4r2). The split bounds blast radius and attributes spend. It is
# not a security boundary, and the words here used to imply that it was.
#
# It remains better than putting provider keys on the nodes: the Anthropic key
# never leaves Cloudflare, the token is revocable centrally without rotating it,
# and every request is logged and metered.
#
# Accepted deliberately on 2026-08-15 rather than overlooked. Cloudflare's own
# guidance is separate accounts or a Worker-side binding per domain; both were
# weighed and judged not worth the cost for the pilot. Revisit if the NGED
# contract requires demonstrable tenant isolation (wg-8yv.37).

locals {
  # Security domains, matching the project visibility classes. A cell is placed
  # in exactly one.
  ai_domains = {
    oss      = "open source work"
    internal = "internal and product work"
    client   = "restricted client work"
  }
}

resource "cloudflare_ai_gateway" "cell" {
  for_each = local.ai_domains

  account_id = var.cloudflare_account_id
  id         = "${local.name}-${each.key}"

  # A token is REQUIRED. Without this the gateway URL alone is enough to spend
  # money, and that URL travels in environment variables, process listings and
  # crash dumps.
  authentication = true

  # Zero data retention is deliberately OFF, and the reason recorded here before
  # was WRONG in both halves. Corrected against Cloudflare's documentation
  # (wg-90f) rather than left as a plausible-sounding comment:
  #
  #   "ZDR does not control AI Gateway logging. To disable request/response
  #    logging in AI Gateway, update the logging settings separately."
  #
  # So zdr does NOT stop Cloudflare storing bodies, and turning it on would NOT
  # have removed the audit trail. It is an upstream-provider property: it routes
  # traffic through provider endpoints that do not retain, and it applies only
  # to Unified Billing requests using Cloudflare-managed credentials — not BYOK,
  # not other AI Gateway requests. It was never the lever for the shared-log
  # problem it was being weighed against.
  #
  # Left false because the property it does buy — upstream non-retention at
  # OpenAI and Anthropic — is not what the pilot needs, and it silently falls
  # back to the non-ZDR configuration for any provider that lacks support,
  # which is a guarantee that cannot be relied on.
  #
  # The lever that DOES work is per-request: cf-aig-collect-log-payload: false
  # keeps the metadata and drops the bodies. internal/inference sets it by
  # default; see the note on Client.CollectPayloads.
  zdr = false

  # Workers AI billing, declared for the same reason the log settings below are.
  #
  # "unified" spends the prepaid credit balance instead of billing Workers AI
  # separately, which is what makes the frontier open-weight models reachable —
  # on "postpaid" the account is refused whatever the balance.
  #
  # Terraform owns it because Terraform owns this resource. It was first set by
  # scripts/ai_gateway_spend_limits.py, and a plan immediately wanted to revert
  # it: `workers_ai_billing_mode = "unified" -> "postpaid"`. Two owners of one
  # field, and the loser was whichever ran last — with no error either way, just
  # credits quietly going unused again. The script now verifies this rather than
  # setting it, so drift is reported by whichever runs and changed by only one.
  workers_ai_billing_mode = "unified"

  # Log retention, declared explicitly rather than left to the server.
  #
  # Cloudflare fills these in on create. Leaving them undeclared makes every
  # subsequent plan want to null them, which fails the apply and — because the
  # resource is in the same module as the tunnels and DNS — blocks every other
  # infrastructure change behind it.
  log_management          = 10000000
  log_management_strategy = "DELETE_OLDEST"
  logpush                 = false

  # ONE STORE FOR NINE GATEWAYS, six of them other projects.
  #
  # This attribute is why the split below is not a logging boundary. Cloudflare
  # assigns a store per account, not per gateway, and there is no per-gateway
  # store to move to. Observed 2026-09-01, all writing to
  # 10ba352dd98c4f2db387148e7313e451:
  #
  #   workgraph-staging-oss, -internal, -client   ours
  #   openclaw-gateway-prod, datahub-sales,
  #   open-design, flowershow                     unrelated Datopian projects
  #
  # So request and response bodies from the client domain sit in the same store
  # as four other projects' bodies, and anyone who can read the store reads all
  # of them. The mitigation is to stop putting bodies there at all
  # (cf-aig-collect-log-payload: false, set by internal/inference), not to try to
  # separate the store.
  #
  # The log store, declared for exactly the reason the block above is declared.
  #
  # Cloudflare assigns a store on create and this attribute was never declared,
  # so every plan wanted to set it to null — which would DETACH the log store.
  # Those logs are what scripts/cost_by_role.py reads and what the evidence pack
  # depends on to answer "what did this agent actually ask for", so the change
  # nobody intended was also the expensive one. It sat in the plan for weeks,
  # riding along with any unrelated apply.
  #
  # An empty value leaves it unmanaged, which is the state a NEW environment is
  # in before Cloudflare has assigned one. After the first apply, read the
  # assigned id and record it here — otherwise the same silent removal comes
  # back on the next plan.
  store_id = var.ai_gateway_store_id != "" ? var.ai_gateway_store_id : null

  # Logs are the spend and behaviour record for the evidence pack. They are also
  # the only way to answer "what did this agent actually ask for" after the
  # fact (plan section 20.2).
  collect_logs = true

  # No caching of model responses. Two agents asking the same question in
  # different security domains must not be able to observe each other through a
  # shared cache, and a stale answer to a code question is worse than a slow one.
  cache_ttl                  = 0
  cache_invalidate_on_update = false

  # Rate limiting is a blast radius control, not a cost control: it bounds how
  # fast a stuck agent loop can burn budget before the spend limit notices.
  rate_limiting_interval  = 60
  rate_limiting_limit     = var.ai_gateway_requests_per_minute
  rate_limiting_technique = "sliding"

  # The spend limit is NOT set here, and must not be added back.
  #
  # The provider serialises the rule field as limit_type while the API requires
  # limitType, so every apply fails with "7001 Required" on
  # body.spend_limits.rules.0.limitType. Worse, the initial create reports
  # success and silently stores nothing: Terraform's state showed a $100 limit,
  # complete with a server-assigned rule ID, while the gateway had none.
  #
  # The budget is therefore applied by scripts/ai_gateway_spend_limits.py, which
  # sends the correct field name and — the part that matters — reads the limit
  # back and fails if it is absent. A budget believed to be in force but missing
  # is worse than no budget, because nobody goes looking for it.

  lifecycle {
    # The spend limit is managed by scripts/ai_gateway_spend_limits.py, so
    # Terraform must leave it alone.
    #
    # Without this, a plan sees a limit it does not declare and proposes
    # "limit = 100 -> null": a successful apply would silently remove the
    # budget. That it currently fails instead is luck, not safety.
    #
    # The script is the authority for this one field, and it verifies by reading
    # back. Everything else about the gateway stays in Terraform.
    ignore_changes = [spend_limits]
  }
}
