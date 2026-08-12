# NON-SECRET account-level configuration. Committed per ADR-0015.

cloudflare_account_id = "83025b28472d6aa2bf5ae59f3724aa78"

# Enabling Zero Trust in the dashboard generated "icy-boat-89aa". That string is
# shown to every person who logs in, including clients, and reads like a
# phishing domain. Renamed here while it is still free to do so: no Access
# application exists, no device is enrolled, and no identity-provider callback
# URL has been registered against the old name.
team_name    = "datopian"
display_name = "Datopian"
