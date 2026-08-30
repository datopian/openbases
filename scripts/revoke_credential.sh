#!/usr/bin/env bash
# Emergency revocation: make a credential unusable on the hosts, now.
#
#   scripts/revoke_credential.sh staging github_webhook_secret
#   scripts/revoke_credential.sh staging --all
#
# This is the "a laptop was stolen" path, and it is deliberately not rotation.
# Rotation preserves service: new value in, old value out, nothing breaks.
# Revocation prioritises stopping use over staying up — it removes the credential
# from the host and restarts the service, which will fail closed if that
# credential was required.
#
# THAT IS THE INTENT. A service that keeps running on a credential you are trying
# to revoke has not been revoked. Read the failure as success.
#
# Tested on staging: removing db_app_password leaves the unit unable to start at
# all, because LoadCredential= points at a file that is gone. systemd reports
#
#   control-api.service: Failed to set up credentials: Protocol error
#   Main process exited, code=exited, status=243/CREDENTIALS
#
# which is a stronger guarantee than the application refusing to run: the
# process never starts. 243/CREDENTIALS is the greppable signal — "Protocol
# error" on its own says nothing about what is missing.
#
# What this does NOT do is revoke at the provider, and the distinction matters:
# removing a token from a host stops THIS system using it and does nothing about
# anyone else who has a copy. The provider steps are printed, and they are the
# ones that actually end the credential's life.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENVIRONMENT="${1:?usage: revoke_credential.sh <environment> <name|--all>}"
TARGET="${2:?usage: revoke_credential.sh <environment> <name|--all>}"

case "$ENVIRONMENT" in
  staging)    HOST="ssh-staging.openbases.com" ;;
  production) HOST="ssh.openbases.com" ;;
  *) echo "unknown environment: $ENVIRONMENT" >&2; exit 2 ;;
esac

SSH=(ssh -o ProxyCommand="cloudflared access ssh --hostname %h"
     -o StrictHostKeyChecking=no "root@${HOST}")

if [ "$TARGET" = "--all" ]; then
  NAMES="db_app_password github_webhook_secret"
  echo "!! revoking EVERY host credential in $ENVIRONMENT. The control API will stop serving."
else
  NAMES="$TARGET"
fi

echo "== removing credential source files on $HOST"
for n in $NAMES; do
  # shred, not rm. The file is small and on a real filesystem; leaving recoverable
  # bytes behind is the opposite of the point.
  "${SSH[@]}" "test -f /etc/workgraph/credentials/$n && shred -u /etc/workgraph/credentials/$n && echo '  removed $n' || echo '  $n was not present'" 2>/dev/null
done

echo "== restarting the service so the running process loses its copy"
# The credential lives in a tmpfs the unit mounts. Until the unit restarts, the
# running process still holds it — removing the source file alone revokes
# nothing from a process that already started.
"${SSH[@]}" 'systemctl restart control-api 2>&1 | tail -2; sleep 3; systemctl is-active control-api' 2>/dev/null | sed 's/^/  /'

echo "== state after revocation"
"${SSH[@]}" 'ls /etc/workgraph/credentials/ 2>/dev/null | sed "s/^/  remaining: /" || echo "  none"
  journalctl -u control-api -n 8 --no-pager -o cat | grep -iE "credential sources|missing|error|fatal" | tail -3 | sed "s/^/  /"' 2>/dev/null

cat <<EOF

== the part that actually revokes the credential
Removing it here stops THIS system using it. Anyone else holding a copy is
unaffected until you do the following:

  github_webhook_secret   change it in the GitHub App settings
  db_app_password         ALTER ROLE workgraph_app PASSWORD '<new>'
  github_app_private_key  App settings > Private keys > delete the old key
  ai_gateway_token        Cloudflare > AI Gateway > revoke the token
  google_service_account_key
                          gcloud iam service-accounts keys delete <KEY_ID> \
                            --iam-account=workgraph-events@<PROJECT_ID>.iam.gserviceaccount.com
                          List first with keys list --managed-by=user. Through
                          domain-wide delegation this key reads every Meet
                          recording and Drive file in the tenant, so an old key
                          left valid is a live credential, not clutter.
  cloudflare_api_token    Cloudflare > Manage Account > API Tokens > revoke
  hcloud_token            Hetzner Console > Security > API tokens

== to restore service
  scripts/rotate_credential.sh $ENVIRONMENT <name>    # mint a replacement
  cd infra/ansible && ../../scripts/with_secrets.sh $ENVIRONMENT \\
    ansible-playbook -i inventory/$ENVIRONMENT.yml site.yml
EOF
