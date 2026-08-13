#!/usr/bin/env bash
# Migrate OpenTofu state from local disk into the R2 backend.
#
# Run once per root module, after bucket-scoped R2 credentials exist. Safe to
# re-run: an already-migrated module reports no change.
set -euo pipefail

cd "$(dirname "$0")/.."

: "${R2_ACCESS_KEY_ID:?set R2_ACCESS_KEY_ID (see infra/tofu/README.md)}"
: "${R2_SECRET_ACCESS_KEY:?set R2_SECRET_ACCESS_KEY}"
: "${TOFU_STATE_PASSPHRASE:?set TOFU_STATE_PASSPHRASE}"

# The S3 backend reads AWS_* names; R2 credentials are supplied under their own
# names so it is obvious they are not AWS.
export AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID"
export AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY"

export TF_ENCRYPTION="
key_provider \"pbkdf2\" \"main\" {
  passphrase = \"$TOFU_STATE_PASSPHRASE\"
}
method \"aes_gcm\" \"main\" {
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
"

DIR="${1:-}"
if [ -z "$DIR" ]; then
  echo "usage: $0 <account|envs/staging|envs/production>" >&2
  exit 2
fi

TARGET="infra/tofu/$DIR"
[ -d "$TARGET" ] || { echo "no such module: $TARGET" >&2; exit 2; }

echo "==> migrating $DIR"
tofu -chdir="$TARGET" init -backend-config=backend.hcl -migrate-state -input=false

echo "==> verifying state is readable from R2"
tofu -chdir="$TARGET" state list

echo "==> confirming no drift after migration"
if tofu -chdir="$TARGET" plan -input=false -detailed-exitcode >/dev/null 2>&1; then
  echo "    no drift"
else
  case $? in
    2) echo "    DRIFT DETECTED — inspect before proceeding"; exit 1 ;;
    *) echo "    plan failed — inspect before proceeding"; exit 1 ;;
  esac
fi

echo
echo "$DIR migrated. The local terraform.tfstate is now a stale copy; delete it"
echo "once you have confirmed the R2 copy works:"
echo "  rm $TARGET/terraform.tfstate $TARGET/terraform.tfstate.backup"
