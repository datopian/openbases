#!/usr/bin/env bash
# Create the R2 bucket that holds OpenTofu remote state.
#
# This is the one resource that cannot be created by OpenTofu itself, because
# OpenTofu needs it before it can store state. It is a committed, idempotent
# script rather than a remembered manual step (plan section 16.5): running it
# twice is safe, and the bucket's existence is represented in Git.
set -euo pipefail

: "${CLOUDFLARE_API_TOKEN:?set CLOUDFLARE_API_TOKEN}"
: "${CLOUDFLARE_ACCOUNT_ID:?set CLOUDFLARE_ACCOUNT_ID}"

ENVIRONMENT="${1:-}"
if [ -z "$ENVIRONMENT" ]; then
  echo "usage: $0 <staging|production>" >&2
  exit 2
fi
case "$ENVIRONMENT" in
  staging|production) ;;
  *) echo "environment must be staging or production" >&2; exit 2 ;;
esac

BUCKET="workgraph-tfstate-${ENVIRONMENT}"
API="https://api.cloudflare.com/client/v4/accounts/${CLOUDFLARE_ACCOUNT_ID}/r2/buckets"

exists=$(curl -s -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" "${API}/${BUCKET}" \
         | jq -r '.success // false')

if [ "$exists" = "true" ]; then
  echo "bucket ${BUCKET} already exists — nothing to do"
  exit 0
fi

echo "creating ${BUCKET} in weur"
response=$(curl -s -X POST "$API" \
  -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
  -H "Content-Type: application/json" \
  --data "{\"name\":\"${BUCKET}\",\"locationHint\":\"weur\"}")

if [ "$(echo "$response" | jq -r '.success')" != "true" ]; then
  echo "failed to create bucket:" >&2
  echo "$response" | jq -r '.errors' >&2
  exit 1
fi

echo "created ${BUCKET}"
echo
echo "Next: mint R2 S3 credentials SCOPED TO THIS BUCKET (not account-wide),"
echo "put them in R2_ACCESS_KEY_ID / R2_SECRET_ACCESS_KEY, then run:"
echo "  tofu init -backend-config=bucket=${BUCKET} ..."
