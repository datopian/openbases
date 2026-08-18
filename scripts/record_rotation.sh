#!/usr/bin/env bash
# Record that a credential was rotated, in credential_registry.
#
#   scripts/record_rotation.sh staging github_webhook_secret
#
# Separate from rotate_credential.sh on purpose. Most rotations have a provider
# step a script cannot finish — deleting the old GitHub App key, revoking the old
# Cloudflare token — and a registry that says "rotated today" while the old
# credential is still valid is worse than one that says "never rotated". It is
# the difference between a record and a wish.
#
# Refuses to record a credential that is not registered: an unregistered
# credential is one nobody owns, and silently succeeding would hide that.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENVIRONMENT="${1:?usage: record_rotation.sh <environment> <name>}"
NAME="${2:?usage: record_rotation.sh <environment> <name>}"

case "$ENVIRONMENT" in
  staging)    HOST="ssh-staging.openbases.com" ;;
  production) HOST="ssh.openbases.com" ;;
  *) echo "unknown environment: $ENVIRONMENT" >&2; exit 2 ;;
esac

# Run on the control node as the database owner. The rotation record is not
# something an execution cell should be able to write, so there is no API for it.
result=$(ssh -o ProxyCommand="cloudflared access ssh --hostname %h" \
             -o StrictHostKeyChecking=no "root@${HOST}" \
  "su - postgres -c \"psql -At -d workgraph -c \\\"SELECT system_record_rotation('${NAME}', now())\\\"\"" 2>/dev/null | tr -d '[:space:]')

case "$result" in
  t) echo "recorded: $NAME rotated just now" ;;
  f) echo "REFUSED: '$NAME' is not in credential_registry." >&2
     echo "Add it to a migration first — a credential nobody registered is one nobody owns." >&2
     exit 1 ;;
  *) echo "could not record the rotation (got: '${result:-nothing}')" >&2; exit 1 ;;
esac

ssh -o ProxyCommand="cloudflared access ssh --hostname %h" -o StrictHostKeyChecking=no "root@${HOST}" \
  "su - postgres -c \"psql -d workgraph -c 'SELECT name, state, due_at FROM credential_rotation_due WHERE state <> \\\$\\\$ok\\\$\\\$ ORDER BY name'\"" 2>/dev/null | head -20
