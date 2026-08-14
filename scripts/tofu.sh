#!/usr/bin/env bash
# Run OpenTofu against an environment with state encryption and R2 configured.
#
# Encryption is configured through TF_ENCRYPTION rather than an `encryption`
# block in HCL: a block referencing var. cannot be evaluated statically, which
# breaks every command that runs before variables resolve.
#
# This exists because the shape of that value is easy to get wrong by hand — a
# collapsed block or a renamed key provider fails in a way that reads like a
# syntax error in the configuration rather than in the environment variable.
#
# Usage: scripts/tofu.sh <staging|production> <plan|apply|...> [args...]
set -euo pipefail

env_name="${1:?usage: tofu.sh <environment> <command> [args...]}"
shift

: "${TOFU_STATE_PASSPHRASE:?set it, or source ~/.config/datopian-workgraph/credentials.env}"
: "${R2_ACCESS_KEY_ID:?R2 credentials are needed for the state backend}"
: "${R2_SECRET_ACCESS_KEY:?R2 credentials are needed for the state backend}"

export TF_ENCRYPTION='
key_provider "pbkdf2" "main" {
  passphrase = "'"$TOFU_STATE_PASSPHRASE"'"
}
method "aes_gcm" "main" {
  keys = key_provider.pbkdf2.main
}
state {
  method   = method.aes_gcm.main
  enforced = true
}
plan {
  method   = method.aes_gcm.main
  enforced = true
}
'

export AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID"
export AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root/infra/tofu/envs/$env_name"
exec tofu "$@"
