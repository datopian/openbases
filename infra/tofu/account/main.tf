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

  # MFA is NOT set here yet, and that is a deliberate gap tracked as wg-8yv.42.
  #
  # Plan section 8.1 requires MFA at the identity edge. Cloudflare rejects
  # mfa_required_for_all_apps unless mfa_config is also set, because that flag
  # turns on Cloudflare's own INDEPENDENT second factor — an extra enrolment on
  # top of whatever the identity provider already enforces.
  #
  # The alternative is to let Google Workspace enforce 2FA and have Cloudflare
  # trust it through the AMR claim. That is less friction and uses the factor
  # people already have, but it moves the control into Google Workspace admin
  # settings, which are not represented in this repository.
  #
  # Both are defensible; the choice belongs to Datopian, not to a default.

  # Refuse any request that matches no Access application, instead of letting it
  # through to an origin. Fail closed.
  deny_unmatched_requests = true

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
