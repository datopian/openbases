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

  # Plan section 8.1 requires MFA at the identity edge. Enforcing it for every
  # application, rather than per application, means a new Access application
  # cannot be created without it by omission.
  mfa_required_for_all_apps = true

  # Refuse any request that matches no Access application, instead of letting it
  # through to an origin. Fail closed.
  deny_unmatched_requests = true

  # Do not skip the login page. Auto-redirect is convenient, but it removes the
  # screen that tells a user which organisation is asking for their identity —
  # which is exactly the check that makes a phishing domain noticeable.
  auto_redirect_to_identity = false
}
