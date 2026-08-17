#!/usr/bin/env bash
# Run a command with this environment's secrets in its environment.
#
#   scripts/with_secrets.sh staging ./scripts/tofu.sh staging plan
#   scripts/with_secrets.sh staging python3 scripts/ai_gateway_spend_limits.py staging
#
# This replaces `set -a; . ~/.config/datopian-workgraph/credentials.env; set +a`,
# which is the bootstrap mechanism WP-B3 exists to retire. Two differences
# matter:
#
#   the values come from an encrypted file that is IN the repository, so there
#   is one copy, reviewed, versioned, and not silently different on someone
#   else's laptop;
#
#   they are exported into one child process rather than into the interactive
#   shell, so they are gone when the command returns and cannot leak into the
#   next thing you run by accident.
#
# Decrypted material is never written to disk. SOPS output goes into a variable
# and the variable dies with the process.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENVIRONMENT="${1:?usage: with_secrets.sh <staging|production> <command> [args...]}"
shift
[ "$#" -gt 0 ] || { echo "usage: with_secrets.sh <environment> <command> [args...]" >&2; exit 2; }

FILE="$ROOT/infra/secrets/${ENVIRONMENT}.enc.yaml"
[ -f "$FILE" ] || { echo "no secrets file for '$ENVIRONMENT' at $FILE" >&2; exit 1; }

: "${SOPS_AGE_KEY_FILE:=$HOME/.config/datopian-workgraph/age-workgraph.key}"
export SOPS_AGE_KEY_FILE
if [ ! -f "$SOPS_AGE_KEY_FILE" ]; then
  echo "no age key at $SOPS_AGE_KEY_FILE" >&2
  echo "It is the private half of the recipient in .sops.yaml. See infra/secrets/README.md." >&2
  exit 1
fi
command -v sops >/dev/null || { echo "sops is not installed (brew install sops age)" >&2; exit 1; }

# The mapping from file keys to the variable names the tooling already expects.
# Written out rather than derived, because a rename in either place should be a
# visible edit here and not a silently missing variable at apply time.
plain="$(sops -d "$FILE" 2>/dev/null)" || {
  echo "could not decrypt $FILE — is $SOPS_AGE_KEY_FILE the right key?" >&2
  exit 1
}

read_key() {
  # Flat scalars only. The one block scalar (the PEM) is handled separately.
  printf '%s\n' "$plain" | sed -n "s/^$1: \{0,1\}//p" | head -1
}

export HCLOUD_TOKEN="$(read_key hcloud_token)"
export HCLOUD_PROJECT="$(read_key hcloud_project)"
export HCLOUD_LOCATION="$(read_key hcloud_location)"
export CLOUDFLARE_API_TOKEN="$(read_key cloudflare_api_token)"
export CLOUDFLARE_ACCOUNT_ID="$(read_key cloudflare_account_id)"
export CLOUDFLARE_ZONE_ID="$(read_key cloudflare_zone_id)"
export CLOUDFLARE_ZONE_NAME="$(read_key cloudflare_zone_name)"
export TOFU_STATE_PASSPHRASE="$(read_key tofu_state_passphrase)"
export WG_DB_APP_PASSWORD="$(read_key db_app_password)"
export GITHUB_APP_ID="$(read_key github_app_id | tr -d '"')"
export GITHUB_APP_INSTALLATION_ID="$(read_key github_app_installation_id | tr -d '"')"
export GITHUB_APP_CLIENT_ID="$(read_key github_app_client_id)"
export GITHUB_WEBHOOK_SECRET="$(read_key github_webhook_secret)"
export WG_AI_GATEWAY_TOKEN="$(read_key ai_gateway_token)"
export WG_ACCESS_AUD_STAGING="$(read_key access_aud_staging)"
export TF_VAR_google_workspace_client_secret="$(read_key google_workspace_client_secret)"
export R2_ACCESS_KEY_ID="$(read_key r2_access_key_id)"
export R2_SECRET_ACCESS_KEY="$(read_key r2_secret_access_key)"
export R2_S3_ENDPOINT="https://${CLOUDFLARE_ACCOUNT_ID}.r2.cloudflarestorage.com"

# The GitHub App key is a multi-line block scalar, and several tools want a PATH
# rather than the contents. It is materialised into a private temporary file for
# the lifetime of the command only, and removed on every exit path including a
# signal — a key left in /tmp is exactly the thing being retired here.
if printf '%s\n' "$plain" | grep -q '^github_app_private_key: |'; then
  keydir="$(mktemp -d)"
  chmod 700 "$keydir"
  trap 'rm -rf "$keydir"' EXIT INT TERM
  printf '%s\n' "$plain" \
    | sed -n '/^github_app_private_key: |/,/^[^ ]/p' \
    | sed '1d;/^[^ ]/d;s/^  //' > "$keydir/github-app.pem"
  chmod 600 "$keydir/github-app.pem"
  export GITHUB_APP_PRIVATE_KEY_PATH="$keydir/github-app.pem"
fi

unset plain

# Deliberately NOT exec. exec replaces this shell, so the EXIT trap never runs
# and the temporary private key survives the command — which is precisely the
# leak this script exists to prevent. Written with exec first, and caught by a
# test that checked the file was gone afterwards rather than assuming it.
#
# The cost is one extra process in the tree and manual status propagation. The
# trap fires on EXIT, INT and TERM, so the key is removed on every path.
"$@"
status=$?
exit "$status"
