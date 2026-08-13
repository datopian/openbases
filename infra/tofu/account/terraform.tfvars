# NON-SECRET account-level configuration. Committed per ADR-0015.

cloudflare_account_id = "83025b28472d6aa2bf5ae59f3724aa78"

# Enabling Zero Trust in the dashboard generated "icy-boat-89aa". That string is
# shown to every person who logs in, including clients, and reads like a
# phishing domain. Renamed here while it is still free to do so: no Access
# application exists, no device is enrolled, and no identity-provider callback
# URL has been registered against the old name.
team_name    = "datopian"
display_name = "Datopian"

# Google Workspace identity provider.
#
# The client ID is not a credential — it appears in OAuth redirect URLs — so it
# lives here with the rest of the configuration. The client secret is supplied
# through TF_VAR_google_workspace_client_secret and must never be written to a
# tfvars file; check_infra.py fails the build if it is.
#
# Leave the ID empty until the OAuth client exists in Google Cloud; the resource
# is created only when both the ID and the secret are present, so a fresh clone
# plans cleanly before anyone has been to the console.
google_workspace_client_id = "578082810196-p3j2qgb9kvg2qd9q57pmtotb0qmfq2f9.apps.googleusercontent.com"
google_workspace_domain    = "datopian.com"
