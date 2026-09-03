variable "environment" {
  description = "Deployment environment. Drives naming, sizing and firewall posture."
  type        = string

  validation {
    condition     = contains(["staging", "production"], var.environment)
    error_message = "environment must be staging or production."
  }
}

variable "location" {
  description = "Hetzner location. All nodes in an environment share one location: placement groups do not span locations."
  type        = string
  default     = "fsn1"
}

variable "network_cidr" {
  description = "Private network CIDR. Staging and production use different ranges so a misrouted peer cannot reach the wrong environment."
  type        = string
}

variable "subnet_cidr" {
  description = "Subnet within network_cidr."
  type        = string
}

variable "control_server_type" {
  description = "Hetzner server type for the control node: API, PostgreSQL, worker, observability. Never runs agent worktrees."
  type        = string
  default     = "cx43"
}

variable "execution_server_type" {
  description = "Hetzner server type for the execution node: Gas Town cells, Dolt, worktrees, builds. Production only."
  type        = string
  default     = "cx53"
}

variable "image" {
  description = "Base OS image. Ubuntu LTS."
  type        = string
  default     = "ubuntu-24.04"
}

variable "with_execution_node" {
  description = "Whether to create a separate execution node. Production runs the control plane and agent workloads on different machines so a noisy build cannot starve the system of record. Staging runs a single node."
  type        = bool
  default     = false
}

variable "enable_hetzner_backups" {
  description = "Hetzner daily server backups, an additional disaster-recovery layer. Live backups may be inconsistent and exclude volumes, so this never replaces database-native backup."
  type        = bool
  default     = true
}

variable "admin_ssh_key_ids" {
  description = "Hetzner SSH key IDs installed on the nodes. Used only for break-glass through the Hetzner console; inbound SSH from the internet stays closed."
  type        = list(string)
  default     = []
}

variable "admin_ssh_cidrs" {
  description = <<-EOT
    Source CIDRs permitted to reach SSH.

    Empty by default, and it should stay empty: the origin is reached through
    Cloudflare Tunnel, which connects outbound. Setting this opens a public
    inbound port and must be a deliberate, reviewed, temporary act — it will be
    visible in the plan diff, which is the point.
  EOT
  type        = list(string)
  default     = []
}

variable "cloudflare_account_id" {
  description = "Cloudflare account identifier."
  type        = string
}

variable "cloudflare_zone_id" {
  description = "Cloudflare zone identifier for the hostname this environment serves."
  type        = string
}

variable "hostname" {
  description = "Fully qualified hostname served through Cloudflare Access and Tunnel."
  type        = string
}

variable "access_allowed_emails" {
  description = "Email addresses permitted by the Cloudflare Access policy. An empty list denies everyone, which is the correct failure direction."
  type        = list(string)
  default     = []
}

variable "access_session_duration" {
  description = "Cloudflare Access session lifetime."
  type        = string
  default     = "24h"
}

variable "r2_buckets" {
  description = <<-EOT
    R2 bucket base names created for this environment. The environment name is
    appended.

    "tfstate" is deliberately absent. The state bucket cannot be created by the
    configuration whose state it holds, so scripts/bootstrap_state_bucket.sh
    creates it before the first init. Listing it here as well would mean two
    owners for one bucket, and the apply would fail on a name that already
    exists.
  EOT
  type        = list(string)
  default     = ["backups", "evidence", "audit"]

  validation {
    condition     = !contains(var.r2_buckets, "tfstate")
    error_message = "tfstate is owned by scripts/bootstrap_state_bucket.sh, not by this module."
  }
}

variable "r2_location" {
  description = "R2 bucket location hint. Western Europe keeps client-derived evidence near the Falkenstein nodes and inside the EEA."
  type        = string
  default     = "weur"

  validation {
    condition     = contains(["weur", "eeur", "enam", "wnam", "apac", "oc"], var.r2_location)
    error_message = "r2_location must be one of weur, eeur, enam, wnam, apac, oc."
  }
}

variable "cloudflared_version" {
  description = "Pinned cloudflared release. Bootstrapped by cloud-init and verified against cloudflared_sha256."
  type        = string
  default     = "2026.7.3"
}

variable "cloudflared_sha256" {
  description = "SHA-256 of cloudflared-linux-amd64.deb for cloudflared_version. A mismatch aborts the bootstrap."
  type        = string
  default     = "049777d30f9bf93da6df8bbe31383460eb2aa51a832c6551824d56f9fcc55974"
}

variable "app_port" {
  description = "Local port the tunnel routes to on the control node. The control API binds here."
  type        = number
  default     = 8080
}

variable "api_hostname" {
  description = <<-EOT
    Fully qualified hostname for the token-authenticated API surface, or "" to
    create nothing.

    Separate from `hostname` on purpose (wg-p4h.1). A personal API token has to
    work across the whole /v1 surface, and neither existing shape fits that: a
    per-path Access application would mean one application per endpoint, and the
    comment on cell_token_mint explains why widening one to a prefix is unsafe —
    it "would quietly extend the token's reach to every path underneath it". A
    bypass policy on /v1 of the interface's own hostname would remove Access from
    the entire API including the paths a browser uses.

    So the token surface gets its own name, and the blast radius of the bypass is
    one origin name that only ever serves token-authenticated traffic. ADR-0006's
    "origin closed to inbound Internet traffic" stance gets one named exception
    with a boundary rather than a general hole.

    On this hostname the token is the whole security boundary: no Access session,
    no MFA. That is the price of a credential a tool can hold, and it is why
    wg-p4h.2's hashing, bounded expiry and revoke-on-next-request are not
    optional, and why wg-p4h.9 (per-token rate and spend limits) is sequenced
    before any external client can dispatch.
  EOT
  type        = string
  default     = ""
}

variable "ssh_hostname" {
  description = <<-EOT
    Hostname used to reach the node's SSH over Cloudflare Tunnel, for
    configuration management.

    This opens no port. The connector runs on the host, so it reaches
    localhost:22 from the inside; the firewall stays deny-all. Empty disables
    the path entirely.
  EOT
  type        = string
  default     = ""
}

variable "ssh_hostname_execution" {
  description = <<-EOT
    Hostname for SSH to the execution node, over its own tunnel.

    Separate from ssh_hostname because each node runs its own tunnel. One tunnel
    shared by two nodes routes SSH to whichever connector Cloudflare picks,
    which makes it impossible to target a specific host.
  EOT
  type        = string
  default     = ""
}

variable "ai_gateway_requests_per_minute" {
  type        = number
  description = "Per-gateway request ceiling. Bounds how fast a stuck agent loop can spend before the cost limit reacts."
  default     = 120
}

variable "ai_monthly_budget" {
  type        = number
  description = <<-EOT
    The shared monthly spend POOL, in US dollars, across all security domains.

    It is divided between the gateways by ai_budget_shares rather than applied
    whole to each. Cloudflare enforces a limit per gateway and cannot express
    one pool spanning three, so writing the full figure to each made the real
    ceiling three times the agreed one.

    Zero disables agent spend entirely, which is the correct value until a
    figure has been agreed.
  EOT
  default     = 0

  validation {
    condition     = var.ai_monthly_budget >= 0
    error_message = "A negative budget would be silently treated as no limit."
  }
}

variable "ai_budget_shares" {
  type        = map(number)
  description = <<-EOT
    How the shared monthly pool is divided between security domains, keyed by
    the gateway suffix (oss, internal, client). Must sum to 1.

    Cloudflare enforces a spend limit per gateway and cannot express one pool
    spanning three, and it exposes no spend endpoint to sum them with. Dividing
    the pool makes the arithmetic the enforcement: three limits whose shares add
    to 1 cannot together exceed the budget. Writing the whole figure to each
    gateway instead — the previous behaviour — made the real ceiling three times
    the agreed one.

    The cost is that a domain can be refused while the pool still has room.
    Applied by scripts/ai_gateway_spend_limits.py, not by this configuration:
    the provider serialises the rule field in a shape the API rejects.
  EOT
  default     = {}

  validation {
    condition     = length(var.ai_budget_shares) == 0 || abs(sum(values(var.ai_budget_shares)) - 1) < 0.000001
    error_message = "ai_budget_shares must sum to 1, so the per-gateway limits add up to the pool."
  }
}

# ---------------------------------------------------------------------------
# R2 retention (WP-I2, wg-y9w)
# ---------------------------------------------------------------------------

variable "r2_lock_days" {
  description = <<-EOT
    How long an object in the backup, evidence and audit buckets cannot be
    deleted or overwritten, in days.

    This is the control that makes "the node cannot destroy its own backups"
    true of the BUCKET rather than merely true of the script that writes to it.
    The control node holds a token with object write access, so without a lock
    anything the node can reach it can also delete — and a backup an attacker can
    delete is not a backup.

    Fourteen days, and the number is constrained from above rather than chosen
    freely: a lifecycle rule cannot delete a locked object, so the lock must be
    SHORTER than the retention or nothing ever expires and storage grows for
    ever while appearing managed. With retention at thirty days, the lock has to
    be less than thirty.

    Fourteen leaves clear room for expiry and is longer than any realistic gap in
    noticing that backups have stopped — the monitor alerts on a stale stream
    within thirty-six hours. A compromised node can stop new backups being
    written, which is alerted; it cannot remove the fortnight already stored.
  EOT
  type        = number
  default     = 14

  validation {
    condition     = var.r2_lock_days >= 7
    error_message = "A lock shorter than a week would not survive a weekend, which is when an incident is least likely to be noticed."
  }
}

variable "r2_audit_lock_days" {
  description = <<-EOT
    The same, for the audit and evidence buckets, where the plan requires at
    least twelve months of retention (section 15.1). Audit records are the one
    thing that must survive somebody wanting them gone.
  EOT
  type        = number
  default     = 365
}

variable "r2_daily_retention_days" {
  description = <<-EOT
    How long daily backup objects live before a lifecycle rule expires them.

    Thirty days, matching the monthly prefix: this deployment keeps a flat
    thirty days of backups rather than the plan's thirty daily plus twelve
    monthly.

    Longer than the lock, necessarily: a lifecycle rule cannot delete a locked
    object, so a retention shorter than the lock would silently never take
    effect and storage would grow for ever while appearing to be managed.
  EOT
  type        = number
  default     = 30

  validation {
    condition     = var.r2_daily_retention_days > var.r2_lock_days
    error_message = "Daily retention must exceed the lock period, or the lifecycle rule can never delete anything."
  }
}

variable "r2_monthly_retention_days" {
  description = <<-EOT
    How long the monthly backup copies live. The plan asks for thirty daily and
    twelve monthly (section 15.3); a single age-based rule cannot express both,
    so monthly copies go to their own prefix with their own rule.

    Thirty days, the same as the dailies, which makes this prefix REDUNDANT and
    it is worth saying so rather than leaving a reader to work it out: a monthly
    copy that expires after thirty days means at most one exists at a time, and
    there is no long-term history. This deployment keeps a flat thirty days.

    The prefix and its one extra upload a month are kept because lengthening
    this number is the whole change needed to get twelve-month retention back,
    where deleting the prefix would mean re-deriving it.
  EOT
  type        = number
  default     = 30
}

variable "ai_gateway_store_id" {
  description = <<-EOT
    The AI Gateway log store the gateways write to.

    Assigned by Cloudflare on create, not by us, which is why it is recorded
    rather than generated. Undeclared it reads as a removal on every plan, and
    applying that detaches the logs the cost report and the evidence pack are
    built from.

    All three gateways currently share ONE store, which means the client
    domain's prompts and responses sit alongside the OSS ones. That is the same
    non-isolation wg-4r2 records for tokens, now visible in logging; separating
    them is a change worth making on its own rather than inside a drift fix.

    Empty means unmanaged, for an environment where Cloudflare has not assigned
    one yet.
  EOT
  type        = string
  default     = ""
}

# Managed OAuth on the main Access application (wg-p4h.11, ADR-0028).
#
# What it changes: an unauthenticated non-browser request to var.hostname is
# answered with 401 plus a WWW-Authenticate header pointing at Access's OAuth
# discovery documents, instead of a redirect to the login page. Browser traffic
# is unaffected -- a person still gets the same login -- so the blast radius is
# limited to clients that were previously unable to authenticate at all.
#
# Default true because the remote MCP server is useless without it, and the
# environments file can turn it off for one environment without a code change.
variable "access_managed_oauth" {
  description = "Enable Cloudflare Access Managed OAuth on the main application, so hosted MCP clients can authenticate."
  type        = bool
  default     = true
}

# Redirect URIs allowed for clients that register dynamically with Access.
#
# Empty by default and NOT a guess. Claude's hosted clients call a connector
# from Anthropic's cloud and their callback URI is not publicly documented;
# reading it from the Access authentication log on the first real connection
# attempt and adding it here is the honest order. localhost and loopback are
# allowed separately in main.tf and cover the command-line clients without
# needing anything in this list.
variable "access_oauth_allowed_redirect_uris" {
  description = "HTTPS redirect URIs permitted for dynamically registered OAuth clients. Must be exact or end in /* for sub-paths."
  type        = list(string)
  default     = []
}
