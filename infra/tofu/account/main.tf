# Account-level Cloudflare Zero Trust configuration.
#
# This is a singleton per Cloudflare account, so it lives here rather than in
# the environment module: that module is instantiated once per environment and
# two instances would fight over one organisation.
#
# The organisation already exists — enabling Zero Trust in the dashboard creates
# it with a generated team domain — so it is imported rather than created.

import {
  to = cloudflare_zero_trust_organization.this
  id = var.cloudflare_account_id
}

resource "cloudflare_zero_trust_organization" "this" {
  account_id = var.cloudflare_account_id

  # The API stores and returns the FULL domain, so the full domain is what gets
  # written. Setting the bare label here would either be rejected or normalised
  # server-side, and a normalised value produces a diff on every subsequent plan
  # forever.
  #
  # Externally visible on every login screen, and the JWT issuer the control API
  # validates against (WG_ACCESS_TEAM_DOMAIN in internal/config).
  auth_domain = "${var.team_name}.cloudflareaccess.com"
  name        = var.display_name

  session_duration                   = var.session_duration
  user_seat_expiration_inactive_time = var.user_seat_expiration_inactive_time

  # MFA is deliberately NOT configured here. Decided 2026-08-12 (wg-8yv.42):
  # Google Workspace enforces 2FA and Cloudflare trusts it.
  #
  # Setting mfa_required_for_all_apps would enable Cloudflare's own INDEPENDENT
  # second factor — a second enrolment on top of the Google 2FA people already
  # have — and Cloudflare refuses the flag without an mfa_config anyway.
  #
  # The cost of this choice, stated plainly so it is not forgotten: the control
  # now lives in Google Workspace admin settings, which are not in this
  # repository and are therefore invisible to drift detection. Two follow-ups
  # exist to stop that being an act of faith — wg-8yv.45 adds an Access policy
  # rule requiring the identity provider to ASSERT MFA through the AMR claim,
  # and wg-8yv.46 captures Workspace enforcement for the go-live evidence pack.

  # DISABLED 2026-08-12 pending investigation (wg-8yv.47).
  #
  # Setting this true correlated with 403 error 1050 across live zones on this
  # account. It was intended to fail closed for Workgraph hostnames, but this is
  # an ACCOUNT-level setting on an account carrying ~50 production zones, and the
  # blast radius was not what the name implies. Do not re-enable without
  # establishing exactly which traffic it evaluates.
  deny_unmatched_requests = false

  # Do not skip the login page. Auto-redirect is convenient, but it removes the
  # screen that tells a user which organisation is asking for their identity —
  # which is exactly the check that makes a phishing domain noticeable.
  auto_redirect_to_identity = false
}

# Google Workspace as the identity provider.
#
# Chosen over the generic "Google" integration because it restricts sign-in to
# one Workspace domain and can read group membership. Groups are what WP-C2 will
# map onto Workgraph roles, so taking the generic integration now would mean
# redoing this later.
#
# Created only once credentials are supplied, so a fresh clone plans cleanly
# before anyone has been to the Google Cloud console.
resource "cloudflare_zero_trust_access_identity_provider" "google_workspace" {
  count = var.google_workspace_client_id != "" && var.google_workspace_client_secret != "" ? 1 : 0

  account_id = var.cloudflare_account_id
  name       = "Google Workspace"
  type       = "google-apps"

  config = {
    client_id     = var.google_workspace_client_id
    client_secret = var.google_workspace_client_secret
    # Restricts authentication to this Workspace domain. Without it, any Google
    # account could reach the login step.
    apps_domain = var.google_workspace_domain
  }
}
