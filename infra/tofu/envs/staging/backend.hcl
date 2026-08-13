# Remote state on Cloudflare R2, via the S3-compatible endpoint.
#
# Non-secret: bucket, key and endpoint are identifiers. The credentials come
# from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY in the environment, sourced
# from R2_ACCESS_KEY_ID and R2_SECRET_ACCESS_KEY.
#
# Usage:
#   tofu init -backend-config=backend.hcl -migrate-state

bucket = "workgraph-tfstate-staging"
key    = "staging/terraform.tfstate"

# R2 has no regions; "auto" is required by the S3 backend's validation.
region = "auto"

endpoints = {
  s3 = "https://83025b28472d6aa2bf5ae59f3724aa78.r2.cloudflarestorage.com"
}

# R2 is S3-compatible but not S3. These skips are required, not optional:
# R2 has no STS, no account-id lookup, no region list, and rejects the
# additional checksum headers newer AWS SDKs send by default.
skip_credentials_validation = true
skip_region_validation      = true
skip_requesting_account_id  = true
skip_metadata_api_check     = true
skip_s3_checksum            = true
use_path_style              = true
