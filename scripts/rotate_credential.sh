#!/usr/bin/env bash
# Rotate one credential: change it at the provider, in SOPS, and on the hosts.
#
#   scripts/rotate_credential.sh staging github_webhook_secret
#   scripts/rotate_credential.sh staging db_app_password --value "$(openssl rand -base64 32)"
#
# Rotation has two halves and the order matters. Some credentials can be changed
# at the provider and at rest independently (a database password: change it in
# PostgreSQL, then deploy). Others cannot, because there is a window where the
# two disagree and traffic fails (a webhook secret: GitHub signs with the new one
# the instant you save it, so the host must have it FIRST).
#
# So this script does the mechanical half — generate, re-encrypt, record — and
# prints the provider half with the correct ordering for that specific
# credential. It deliberately does not automate the provider side for
# everything: a script that half-rotates a credential and dies leaves an outage
# whose cause is invisible.
#
# What it always does:
#   generates a new value, or takes one you supply
#   re-encrypts infra/secrets/<env>.enc.yaml with it
#   tells you the deploy command and the provider steps, in order
#   records the rotation in credential_registry once you confirm
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

ENVIRONMENT="${1:?usage: rotate_credential.sh <environment> <name> [--value V] [--record-only]}"
NAME="${2:?which credential? see: scripts/rotate_credential.sh <env> --list}"
shift 2

VALUE=""
RECORD_ONLY=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --value) VALUE="${2:?--value needs a value}"; shift 2 ;;
    --record-only) RECORD_ONLY=1; shift ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

FILE="infra/secrets/${ENVIRONMENT}.enc.yaml"
[ -f "$FILE" ] || { echo "no secrets file at $FILE" >&2; exit 1; }
: "${SOPS_AGE_KEY_FILE:=$HOME/.config/datopian-workgraph/age-workgraph.key}"
export SOPS_AGE_KEY_FILE
command -v sops >/dev/null || { echo "sops is not installed" >&2; exit 1; }

# The provider half, per credential. Printed rather than executed, in the order
# that avoids a window where the provider and the host disagree.
provider_steps() {
  case "$1" in
    github_webhook_secret) cat <<'EOF'
  THIS ROTATION HAS A WINDOW, and no ordering removes it.
  Exactly one secret is in force on each side and they must match, so whichever
  you change first, deliveries fail until the other catches up. An earlier
  version of this script claimed deploying first avoided that; it does not.
    1. deploy (below), then immediately
    2. App settings > Webhook secret > save the same value
    3. Advanced tab > redeliver a recent event, confirm 202
  Deliveries rejected in between are not lost: GitHub retries, and a redelivery
  from the Advanced tab recovers anything that exhausted its retries.
  The real fix is accepting the previous secret alongside the current one during
  a rotation, which makes this windowless. Filed as a follow-up.
EOF
      ;;
    db_app_password) cat <<'EOF'
  ORDER MATTERS: change PostgreSQL FIRST, then deploy.
  The running service holds an open pool; changing the password does not close
  it, so there is no outage while you deploy.
    1. psql: ALTER ROLE workgraph_app PASSWORD '<new value>';
    2. deploy (below) — the service restarts and picks up the credential
    3. confirm: journalctl -u control-api | grep "credential sources"
EOF
      ;;
    github_app_private_key) cat <<'EOF'
  PARTLY MANUAL: GitHub does not expose private key creation over the API.
    1. App settings > Private keys > Generate a private key (both keys now work)
    2. sops infra/secrets/<env>.enc.yaml and replace github_app_private_key
    3. deploy (below)
    4. confirm a token still mints, then DELETE the old key in App settings
  Step 4 is the rotation. Generating a new key without deleting the old one
  leaves two valid keys, which is more exposure than before, not less.
EOF
      ;;
    ai_gateway_token|cloudflare_api_token) cat <<'EOF'
  ORDER MATTERS: create the new token BEFORE revoking the old one.
    1. Cloudflare dashboard > create a new token with the same permissions
    2. put it in SOPS (this script, with --value) and deploy
    3. confirm traffic works, then revoke the old token
  Cloudflare tokens cannot be scoped to a single AI Gateway (wg-4r2), so the
  gateway token reaches all three either way.
EOF
      ;;
    *) cat <<'EOF'
  No specific ordering recorded for this credential. Before rotating, decide
  which of these it is, because getting it wrong causes an outage:
    - the provider accepts both old and new for a while  -> either order
    - the provider switches instantly                    -> hosts first
    - the host holds a long-lived connection             -> provider first
  Then add it to provider_steps() in this script so the next person knows.
EOF
      ;;
  esac
}

echo "== rotating $NAME in $ENVIRONMENT"

if [ "$RECORD_ONLY" -eq 0 ]; then
  # Only values this script knows how to generate safely are generated. A key
  # with structure — a PEM, an OAuth secret issued by a provider — cannot be
  # invented locally, and pretending otherwise produces a confident failure.
  if [ -z "$VALUE" ]; then
    case "$NAME" in
      github_webhook_secret|db_app_password)
        VALUE="$(openssl rand -hex 32)"
        echo "  generated a new 64-character value" ;;
      *)
        echo "  this credential cannot be generated locally; supply it with --value" >&2
        echo
        provider_steps "$NAME"
        exit 2 ;;
    esac
  fi

  plain="$(sops -d "$FILE")" || { echo "could not decrypt $FILE" >&2; exit 1; }
  printf '%s\n' "$plain" | grep -q "^${NAME}:" || {
    echo "  $NAME is not a key in $FILE" >&2; exit 1; }

  tmp="$(mktemp)"
  chmod 600 "$tmp"
  trap 'rm -f "$tmp"' EXIT INT TERM
  # Substituted in Python rather than with sed, so a value containing / & or \
  # cannot be read as replacement syntax. A freshly generated secret is exactly
  # where those characters turn up.
  printf '%s\n' "$plain" | NEW_NAME="$NAME" NEW_VALUE="$VALUE" python3 -c '
import os, sys
name, value = os.environ["NEW_NAME"], os.environ["NEW_VALUE"]
out, replaced = [], False
for line in sys.stdin.read().splitlines():
    if line.startswith(name + ":") and not replaced:
        out.append(f"{name}: {value}")
        replaced = True
    else:
        out.append(line)
if not replaced:
    sys.exit(f"{name} not found")
print("\n".join(out))
' > "$tmp" || exit 1

  sops -e --filename-override "$FILE" "$tmp" > "$FILE.new" && mv "$FILE.new" "$FILE"
  rm -f "$tmp"
  echo "  re-encrypted $FILE"
  python3 scripts/check_secrets_encrypted.py >/dev/null || {
    echo "  the encrypted file no longer passes its own check; NOT deploying" >&2; exit 1; }
  echo "  encryption check passed"
fi

cat <<EOF

== provider steps
$(provider_steps "$NAME")

== deploy
  cd infra/ansible && ../../scripts/with_secrets.sh $ENVIRONMENT \\
    ansible-playbook -i inventory/$ENVIRONMENT.yml site.yml

== record it, once the rotation is actually complete
  scripts/record_rotation.sh $ENVIRONMENT $NAME

Recording is separate on purpose. A rotation marked done before the provider
side is finished is worse than one not recorded at all, because the registry
then says a credential is fresh when it is half-changed.
EOF
